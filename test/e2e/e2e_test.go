//go:build e2e

// Package e2e contains real-kernel integration tests for go-tcmu.
// These tests require root privileges and the target_core_user kernel module.
// Run with: sudo go test -tags e2e -race -count=1 -v ./test/e2e/
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	tcmu "github.com/uiscsi/go-tcmu"
)

// atomicCounter provides unique HBA numbers to avoid conflicts between tests.
var hbaCounter atomic.Uint32

func init() {
	hbaCounter.Store(42)
}

// memoryRW is a simple in-memory ReadWriterAt backed by a byte slice.
type memoryRW struct {
	buf []byte
}

func newMemoryRW(size int) *memoryRW {
	return &memoryRW{buf: make([]byte, size)}
}

func (m *memoryRW) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || int(off) >= len(m.buf) {
		return 0, fmt.Errorf("ReadAt: offset %d out of range [0, %d)", off, len(m.buf))
	}
	n := copy(p, m.buf[off:])
	return n, nil
}

func (m *memoryRW) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || int(off)+len(p) > len(m.buf) {
		return 0, fmt.Errorf("WriteAt: write [%d, %d) exceeds capacity %d", off, int(off)+len(p), len(m.buf))
	}
	n := copy(m.buf[off:], p)
	return n, nil
}

// ensureModule attempts to load target_core_user if not already present.
func ensureModule(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/sys/kernel/config/target/core"); err == nil {
		// configfs target already mounted — module is loaded.
		return
	}
	out, err := exec.Command("modprobe", "target_core_user").CombinedOutput()
	if err != nil {
		t.Skipf("target_core_user module not available (modprobe failed: %v, output: %s) — skipping E2E test", err, out)
	}
	// Give the module a moment to settle.
	time.Sleep(100 * time.Millisecond)
}

// TestBlockDeviceRoundtrip exercises the full go-tcmu UIO path:
//  1. Creates a TCMU virtual block device via configfs/UIO.
//  2. Writes a known pattern to the block device via the loopback SCSI path.
//  3. Reads it back and verifies byte-for-byte correctness.
//
// This exercises: UIO fd open, mmap, epoll setup, poll goroutine,
// getNextCommand, completeCommand, ring buffer tail advancement.
func TestBlockDeviceRoundtrip(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root — run with sudo go test -tags e2e")
	}
	ensureModule(t)

	const (
		volumeSize = 1024 * 1024 // 1 MiB
		blockSize  = 512
		devPath    = "/tmp/go-tcmu-e2e-test"
		volName    = "go-tcmu-e2e"
	)

	hba := int(hbaCounter.Add(1))

	backend := newMemoryRW(volumeSize)

	handler := &tcmu.SCSIHandler{
		HBA:        hba,
		LUN:        0,
		VolumeName: volName,
		WWN: tcmu.NaaWWN{
			OUI:      "000000",
			VendorID: tcmu.GenerateSerial(volName),
		},
		DataSizes: tcmu.DataSizes{
			VolumeSize: volumeSize,
			BlockSize:  blockSize,
		},
		DevReady: tcmu.SingleThreadedDevReady(tcmu.ReadWriterAtCmdHandler{
			RW: backend,
		}),
		ExternalFabric: false,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := os.MkdirAll(devPath, 0755); err != nil && !os.IsExist(err) {
		t.Fatalf("mkdir %s: %v", devPath, err)
	}
	defer os.RemoveAll(devPath)

	d, err := tcmu.OpenTCMUDevice(ctx, devPath, handler)
	if err != nil {
		t.Fatalf("OpenTCMUDevice: %v", err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	// Locate the block device node created by OpenTCMUDevice.
	devNode := devPath + "/" + volName
	if _, err := os.Stat(devNode); err != nil {
		t.Fatalf("block device %s not found after OpenTCMUDevice: %v", devNode, err)
	}

	// Write a known pattern via the SCSI/loopback path.
	pattern := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, blockSize/4)
	f, err := os.OpenFile(devNode, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open block device %s: %v", devNode, err)
	}
	defer f.Close()

	if _, err := f.Write(pattern); err != nil {
		t.Fatalf("write to block device: %v", err)
	}

	// Read back from offset 0 and verify.
	got := make([]byte, blockSize)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatalf("read from block device: %v", err)
	}

	if !bytes.Equal(got, pattern) {
		t.Errorf("data mismatch after roundtrip:\n  want: %x\n   got: %x", pattern[:16], got[:16])
	}
}
