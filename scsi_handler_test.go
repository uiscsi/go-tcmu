package tcmu

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestWrite(t *testing.T) {
	var tests = []struct {
		desc  string
		s     *SCSICmd
		wrote int
		err   error
	}{
		{
			desc: "out of buffer space",
			s: &SCSICmd{
				vecs:      [][]byte{{0}, {1}},
				offset:    0,
				vecoffset: 0,
			},
			wrote: 0,
			err:   errors.New("out of buffer scsi cmd buffer space"),
		},
		{
			desc: "write 3 bytes 3x1",
			s: &SCSICmd{
				vecs:      [][]byte{{0}, {1}, {2}},
				offset:    0,
				vecoffset: 0,
			},
			wrote: 3,
		},
		{
			desc: "write 3 bytes 1x3",
			s: &SCSICmd{
				vecs:      [][]byte{{0, 1, 2}},
				offset:    0,
				vecoffset: 0,
			},
			wrote: 3,
		},
	}

	for i, tt := range tests {
		b := []byte{0, 1, 2}
		wrote, err := tt.s.Write(b)
		if err != nil || tt.err != nil {
			if want, got := tt.err, err; want.Error() != got.Error() {
				t.Fatalf("[%02d] test %q, unexpected error: %v != %v",
					i, tt.desc, want, got)
			}
			continue
		}
		want, got := tt.wrote, wrote
		if want != got {
			t.Fatalf("[%02d] test %q, unexpected wrote buffer size:\n- want: %v\n-  got: %v",
				i, tt.desc, want, got)
		}
	}

}

func TestRead(t *testing.T) {
	var tests = []struct {
		desc string
		s    *SCSICmd
		read int
		err  error
	}{
		{
			desc: "read exceeded vecs size",
			s: &SCSICmd{
				vecs:      [][]byte{{0}, {1}},
				offset:    0,
				vecoffset: 0,
			},
			read: 0,
			err:  io.EOF,
		},
		{
			desc: "read 3 bytes 3x1",
			s: &SCSICmd{
				vecs:      [][]byte{{0}, {1}, {2}},
				offset:    0,
				vecoffset: 0,
			},
			read: 3,
		},
		{
			desc: "read 3 bytes 1x3",
			s: &SCSICmd{
				vecs:      [][]byte{{0, 1, 2}},
				offset:    0,
				vecoffset: 0,
			},
			read: 3,
		},
	}

	for i, tt := range tests {
		b := []byte{0, 1, 2}
		read, err := tt.s.Read(b)
		if err != nil || tt.err != nil {
			if want, got := tt.err, err; want != got {
				t.Fatalf("[%02d] test %q, unexpected error: %v != %v",
					i, tt.desc, want, got)
			}
			continue
		}
		want, got := tt.read, read
		if want != got {
			t.Fatalf("[%02d] test %q, unexpected read buffer size:\n- want: %v\n-  got: %v",
				i, tt.desc, want, got)
		}
	}

}

type fakeSCSICmdHandler struct {
	SCSICmdHandler
	FakeHandleCommand func(cmd *SCSICmd) (SCSIResponse, error)
}

func (c *fakeSCSICmdHandler) HandleCommand(cmd *SCSICmd) (SCSIResponse, error) {
	return cmd.Ok(), nil
}

func TestDevReady(t *testing.T) {
	var tests = []struct {
		desc    string
		s       SCSICmdHandler
		id      uint16
		threads int
	}{
		{
			desc:    "DevReady test with SingleThreadedDevReady",
			s:       &fakeSCSICmdHandler{},
			id:      1,
			threads: 1,
		},
		{
			desc:    "DevReady test with MultiThreadedDevReady",
			s:       &fakeSCSICmdHandler{},
			id:      1,
			threads: 2,
		},
	}

	for i, tt := range tests {
		var f DevReadyFunc
		if tt.threads > 1 {
			f = MultiThreadedDevReady(tt.s, tt.threads)
		} else {
			f = SingleThreadedDevReady(tt.s)
		}
		cmdChan := make(chan *SCSICmd, 3)
		respChan := make(chan SCSIResponse, 3)
		f(cmdChan, respChan)
		cmd := &SCSICmd{id: tt.id}
		cmdChan <- cmd
		resp := <-respChan
		want, got := tt.id, resp.id
		if want != got {
			t.Fatalf("[%02d] test %q, unexpected command id in response:\n- want: %v\n-  got: %v",
				i, tt.desc, want, got)
		}
		close(cmdChan) // Signal handler goroutine to exit.
		// Handler goroutine closes respChan — drain remaining to let it complete.
		for range respChan {
		}
	}
}

func TestInquiryDeviceType(t *testing.T) {
	// Test 1: DeviceType=0x01 (tape) produces buf[0]=0x01
	cdb := []byte{0x12, 0x00, 0x00, 0x00, 36, 0x00}
	dataBuf := make([]byte, 36)
	cmd := &SCSICmd{
		cdb:  cdb,
		vecs: [][]byte{dataBuf},
	}
	inq := &InquiryInfo{
		VendorID:   "UISCSI",
		ProductID:  "TAPE DRIVE",
		ProductRev: "0001",
		DeviceType: 0x01,
	}
	resp, err := EmulateStdInquiry(cmd, inq)
	if err != nil {
		t.Fatalf("EmulateStdInquiry returned error: %v", err)
	}
	if resp.status != 0 {
		t.Fatalf("expected Ok status, got %d", resp.status)
	}
	if dataBuf[0] != 0x01 {
		t.Fatalf("expected buf[0]=0x01 (tape), got 0x%02x", dataBuf[0])
	}
	// Verify vendor string is still correct
	vendor := string(dataBuf[8:16])
	if vendor != "UISCSI  " {
		t.Fatalf("expected vendor 'UISCSI  ', got %q", vendor)
	}

	// Test 2: Zero-value InquiryInfo -> DeviceType=0x00 (disk, backward compat)
	dataBuf2 := make([]byte, 36)
	cmd2 := &SCSICmd{
		cdb:  cdb,
		vecs: [][]byte{dataBuf2},
	}
	inq2 := &InquiryInfo{}
	resp2, err := EmulateStdInquiry(cmd2, inq2)
	if err != nil {
		t.Fatalf("EmulateStdInquiry returned error: %v", err)
	}
	if resp2.status != 0 {
		t.Fatalf("expected Ok status, got %d", resp2.status)
	}
	if dataBuf2[0] != 0x00 {
		t.Fatalf("expected buf[0]=0x00 (disk), got 0x%02x", dataBuf2[0])
	}
}

func TestPollCancelShutdown(t *testing.T) {
	// Create a pipe to simulate the UIO fd.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := &Device{
		scsi: &SCSIHandler{},
	}
	d.uioFd = int(r.Fd())

	// Create epoll + eventfd.
	d.epollFd, err = unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(d.epollFd)

	d.cancelFd, err = unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(d.cancelFd)

	if err := unix.EpollCtl(d.epollFd, unix.EPOLL_CTL_ADD, d.uioFd,
		&unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(d.uioFd)}); err != nil {
		t.Fatal(err)
	}
	if err := unix.EpollCtl(d.epollFd, unix.EPOLL_CTL_ADD, d.cancelFd,
		&unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(d.cancelFd)}); err != nil {
		t.Fatal(err)
	}

	d.cmdChan = make(chan *SCSICmd, 5)
	d.respChan = make(chan SCSIResponse, 5)

	// Derive cancellable context and store cancel func (mirrors start()).
	ctx, d.ctxCancel = context.WithCancel(ctx)

	// Start watcher + beginPoll goroutines.
	d.wg.Add(2)
	go func() {
		defer d.wg.Done()
		<-ctx.Done()
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], 1)
		unix.Write(d.cancelFd, buf[:]) //nolint:errcheck
	}()
	go d.beginPoll(ctx)

	// Cancel context — should cause watcher to signal eventfd, beginPoll to exit.
	cancel()

	// Wait with timeout — if goroutines leak, this will timeout.
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success — goroutines exited cleanly.
	case <-time.After(3 * time.Second):
		t.Fatal("goroutines did not exit within 3 seconds after context cancellation")
	}
}
