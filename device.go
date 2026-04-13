// tcmu is a package that connects to the TCM in Userspace kernel module, a part of the LIO stack. It provides the
// ability to emulate a SCSI storage device in pure Go.
package tcmu

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	configDirFmt = "/sys/kernel/config/target/core/user_%d"
	scsiDir      = "/sys/kernel/config/target/loopback"
)

type Device struct {
	scsi    *SCSIHandler
	devPath string

	hbaDir     string
	deviceName string

	uioFd    int
	cancelFd int        // eventfd for cancellation
	epollFd  int        // epoll instance
	mapsize  uint64
	mmap     []byte
	cmdChan  chan *SCSICmd
	respChan chan SCSIResponse
	cmdTail  uint32
	wg        sync.WaitGroup
	ctxCancel context.CancelFunc // stored to ensure watcher goroutine exits in Close()

	toClean map[string]bool
}

// WWN provides two WWNs, one for the device itself and one for the loopback
// device created for the kernel.
type WWN interface {
	DeviceID() string
	NexusID() string
}

func (d *Device) logger() *slog.Logger {
	if d.scsi.Logger != nil {
		return d.scsi.Logger
	}
	return slog.Default()
}

// GetDevConfig returns the TCMU config string for this device.
func (d *Device) GetDevConfig() string {
	return fmt.Sprintf("go-tcmu//%s", d.scsi.VolumeName)
}

// Sizes returns the DataSizes (volume size and block size) configured for this device.
func (d *Device) Sizes() DataSizes {
	return d.scsi.DataSizes
}

// BackstorePath returns the configfs backstore path for this device.
// This path is used for external LUN linking when ExternalFabric is true
// (e.g., creating a symlink from a LIO iSCSI target LUN to this backstore).
func (d *Device) BackstorePath() string {
	return path.Join(d.hbaDir, d.scsi.VolumeName)
}

// OpenTCMUDevice creates the virtual device based on the details in the SCSIHandler, eventually creating a device under devPath (eg, "/dev") with the file name scsi.VolumeName.
// The returned Device represents the open device connection to the kernel, and must be closed.
// The provided context controls the lifetime of the poll goroutine; cancel it (or use Close) to shut down cleanly.
func OpenTCMUDevice(ctx context.Context, devPath string, scsi *SCSIHandler) (*Device, error) {
	d := &Device{
		scsi:     scsi,
		devPath:  devPath,
		uioFd:    -1,
		cancelFd: -1,
		epollFd:  -1,
		hbaDir:   fmt.Sprintf(configDirFmt, scsi.HBA),
		toClean:  make(map[string]bool),
	}
	if err := d.preEnableTcmu(); err != nil {
		_ = d.teardown()
		return nil, err
	}
	if err := d.start(ctx); err != nil {
		if d.ctxCancel != nil {
			d.ctxCancel()
			d.wg.Wait()
		}
		if d.cancelFd >= 0 {
			_ = unix.Close(d.cancelFd)
		}
		if d.epollFd >= 0 {
			_ = unix.Close(d.epollFd)
		}
		if d.uioFd >= 0 {
			_ = unix.Close(d.uioFd)
		}
		_ = d.teardown()
		return nil, err
	}
	if !scsi.ExternalFabric {
		if err := d.postEnableTcmu(); err != nil {
			if d.ctxCancel != nil {
				d.ctxCancel()
				d.wg.Wait()
			}
			if d.cancelFd >= 0 {
				_ = unix.Close(d.cancelFd)
			}
			if d.epollFd >= 0 {
				_ = unix.Close(d.epollFd)
			}
			if d.uioFd >= 0 {
				_ = unix.Close(d.uioFd)
			}
			_ = d.teardown()
			return nil, err
		}
	}
	return d, nil
}

// Close shuts down the TCMU device, stops the poll goroutines, and cleans up configfs entries.
func (d *Device) Close() error {
	// Cancel the context — this causes the watcher goroutine to write to eventfd
	// and exit, which in turn causes beginPoll to exit via epoll.
	if d.ctxCancel != nil {
		d.ctxCancel()
	}

	// Wait for all three goroutines (watcher, beginPoll, recvResponse) to exit.
	d.wg.Wait()

	// Clean up file descriptors.
	if d.cancelFd >= 0 {
		unix.Close(d.cancelFd)
		d.cancelFd = -1
	}
	if d.epollFd >= 0 {
		unix.Close(d.epollFd)
		d.epollFd = -1
	}
	if d.uioFd >= 0 {
		unix.Close(d.uioFd)
		d.uioFd = -1
	}

	return d.teardown()
}

func (d *Device) preEnableTcmu() error {
	err := d.writeLines(path.Join(d.hbaDir, d.scsi.VolumeName, "control"), []string{
		fmt.Sprintf("dev_size=%d", d.scsi.DataSizes.VolumeSize),
		fmt.Sprintf("dev_config=%s", d.GetDevConfig()),
		fmt.Sprintf("hw_block_size=%d", d.scsi.DataSizes.BlockSize),
		"nl_reply_supported=-1",
		"async=1",
	})
	if err != nil {
		return err
	}

	return d.writeLines(path.Join(d.hbaDir, d.scsi.VolumeName, "enable"), []string{
		"1",
	})
}

func (d *Device) getSCSIPrefixAndWnn() (string, string) {
	return path.Join(scsiDir, d.scsi.WWN.DeviceID(), "tpgt_1"), d.scsi.WWN.NexusID()
}

func (d *Device) getLunPath(prefix string) string {
	return path.Join(prefix, "lun", fmt.Sprintf("lun_%d", d.scsi.LUN))
}

func (d *Device) postEnableTcmu() error {
	prefix, nexusWnn := d.getSCSIPrefixAndWnn()

	err := d.writeLines(path.Join(prefix, "nexus"), []string{
		nexusWnn,
	})
	if err != nil {
		return err
	}

	lunPath := d.getLunPath(prefix)
	d.logger().Debug("tcmu: creating directory", "path", lunPath)
	if err := os.MkdirAll(lunPath, 0755); err != nil && !os.IsExist(err) {
		return err
	} else if err == nil {
		d.toClean[lunPath] = true
		d.toClean[path.Join(lunPath, d.scsi.VolumeName)] = true
	}

	d.logger().Debug("tcmu: linking lun",
		"from", path.Join(lunPath, d.scsi.VolumeName),
		"to", path.Join(d.hbaDir, d.scsi.VolumeName))
	if err := os.Symlink(path.Join(d.hbaDir, d.scsi.VolumeName), path.Join(lunPath, d.scsi.VolumeName)); err != nil {
		return err
	}
	d.toClean[path.Join(d.hbaDir, d.scsi.VolumeName)] = true

	return d.createDevEntry()
}

func (d *Device) createDevEntry() error {
	if err := os.MkdirAll(d.devPath, 0755); err != nil && !os.IsExist(err) {
		return err
	}

	dev := filepath.Join(d.devPath, d.scsi.VolumeName)

	if _, err := os.Stat(dev); err == nil {
		return fmt.Errorf("device %s already exists, can not create", dev)
	}
	d.toClean[dev] = true

	tgt, _ := d.getSCSIPrefixAndWnn()

	address, err := os.ReadFile(path.Join(tgt, "address"))
	if err != nil {
		return err
	}

	found := false
	matches := []string{}
	globPattern := fmt.Sprintf("/sys/bus/scsi/devices/%s*/block/*/dev", strings.TrimSpace(string(address)))
	for i := 0; i < 30; i++ {
		var err error
		matches, err = filepath.Glob(globPattern)
		if len(matches) > 0 && err == nil {
			found = true
			break
		}

		d.logger().Debug("tcmu: waiting for device", "path", globPattern)
		time.Sleep(1 * time.Second)
	}

	if !found {
		return fmt.Errorf("failed to find %s", globPattern)
	}

	if len(matches) == 0 {
		return fmt.Errorf("failed to find %s", globPattern)
	}

	if len(matches) > 1 {
		return fmt.Errorf("too many matches for %s, found %d", globPattern, len(matches))
	}

	majorMinor, err := os.ReadFile(matches[0])
	if err != nil {
		return err
	}

	parts := strings.Split(strings.TrimSpace(string(majorMinor)), ":")
	if len(parts) != 2 {
		return fmt.Errorf("invalid major:minor string %s", string(majorMinor))
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return err
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return err
	}

	d.logger().Debug("tcmu: creating device", "path", dev, "major", major, "minor", minor)
	return mknod(dev, major, minor)
}

func mknod(device string, major, minor int) error {
	var fileMode os.FileMode = 0600
	fileMode |= syscall.S_IFBLK
	dev := int((major << 8) | (minor & 0xff) | ((minor & 0xfff00) << 12))

	return syscall.Mknod(device, uint32(fileMode), dev)
}

func (d *Device) writeLines(target string, lines []string) error {
	dir := path.Dir(target)
	if stat, err := os.Stat(dir); os.IsNotExist(err) {
		d.logger().Debug("tcmu: creating directory", "path", dir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		d.toClean[dir] = true
	} else if !stat.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}

	for _, line := range lines {
		content := []byte(line + "\n")
		d.logger().Debug("tcmu: configfs write", "target", target, "line", line)
		if err := os.WriteFile(target, content, 0755); err != nil {
			d.logger().Error("tcmu: configfs write failed", "line", line, "target", target, "err", err)
			return err
		}
	}

	return nil
}

func (d *Device) start(ctx context.Context) error {
	if err := d.findDevice(); err != nil {
		return err
	}

	var err error
	d.epollFd, err = unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("tcmu: epoll_create1: %w", err)
	}

	d.cancelFd, err = unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		unix.Close(d.epollFd)
		d.epollFd = -1
		return fmt.Errorf("tcmu: eventfd: %w", err)
	}

	// Register UIO fd for read events.
	if err := unix.EpollCtl(d.epollFd, unix.EPOLL_CTL_ADD, d.uioFd,
		&unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(d.uioFd)}); err != nil {
		unix.Close(d.cancelFd)
		d.cancelFd = -1
		unix.Close(d.epollFd)
		d.epollFd = -1
		return fmt.Errorf("tcmu: epoll_ctl uioFd: %w", err)
	}

	// Register cancel fd for read events.
	if err := unix.EpollCtl(d.epollFd, unix.EPOLL_CTL_ADD, d.cancelFd,
		&unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(d.cancelFd)}); err != nil {
		unix.Close(d.cancelFd)
		d.cancelFd = -1
		unix.Close(d.epollFd)
		d.epollFd = -1
		return fmt.Errorf("tcmu: epoll_ctl cancelFd: %w", err)
	}

	d.cmdChan = make(chan *SCSICmd, 5)
	d.respChan = make(chan SCSIResponse, 5)

	// Derive a cancellable context so Close() can stop the watcher goroutine.
	ctx, cancel := context.WithCancel(ctx)
	d.ctxCancel = cancel

	// Start context cancellation watcher — tracked by WaitGroup.
	d.wg.Add(3)
	go func() {
		defer d.wg.Done()
		<-ctx.Done()
		// Signal poll goroutine via eventfd.
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], 1)
		unix.Write(d.cancelFd, buf[:]) //nolint:errcheck
	}()
	go d.beginPoll(ctx)
	go d.recvResponse(ctx)
	d.scsi.DevReady(d.cmdChan, d.respChan) //nolint:errcheck
	return nil
}

func (d *Device) findDevice() error {
	err := filepath.Walk("/dev", func(path string, i os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if i.IsDir() && path != "/dev" {
			return filepath.SkipDir
		}
		if !strings.HasPrefix(i.Name(), "uio") {
			return nil
		}
		sysfile := fmt.Sprintf("/sys/class/uio/%s/name", i.Name())
		bytes, err := os.ReadFile(sysfile)
		if err != nil {
			return err
		}
		split := strings.SplitN(strings.TrimRight(string(bytes), "\n"), "/", 4)
		if split[0] != "tcm-user" {
			// Not a TCM device
			d.logger().Debug("tcmu: not a tcm-user device", "uio", i.Name())
			return nil
		}
		if split[3] != d.GetDevConfig() {
			// Not our TCM device
			d.logger().Debug("tcmu: not our tcm-user device", "uio", i.Name())
			return nil
		}
		err = d.openDevice(split[1], split[2], i.Name())
		if err != nil {
			return err
		}
		return filepath.SkipDir
	})
	if err == filepath.SkipDir {
		return nil
	}
	return err
}

func (d *Device) openDevice(user string, vol string, uio string) error {
	var err error
	d.deviceName = vol
	// O_NONBLOCK is intentionally omitted: blocking mode is required because epoll handles readiness.
	d.uioFd, err = syscall.Open(fmt.Sprintf("/dev/%s", uio), syscall.O_RDWR|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	bytes, err := os.ReadFile(fmt.Sprintf("/sys/class/uio/%s/maps/map0/size", uio))
	if err != nil {
		return err
	}
	d.mapsize, err = strconv.ParseUint(strings.TrimRight(string(bytes), "\n"), 0, 64)
	if err != nil {
		return err
	}
	d.mmap, err = syscall.Mmap(d.uioFd, 0, int(d.mapsize), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("tcmu: mmap: %w", err)
	}
	d.cmdTail = d.mbCmdTail()
	d.debugPrintMb()
	return nil
}

func (d *Device) debugPrintMb() {
	d.logger().Debug("tcmu: mailbox info",
		"version", d.mbVersion(),
		"mapsize", d.mapsize,
		"flags", d.mbFlags(),
		"cmdrOffset", d.mbCmdrOffset(),
		"cmdrSize", d.mbCmdrSize(),
		"cmdHead", d.mbCmdHead(),
		"cmdTail", d.mbCmdTail(),
	)
}

func (d *Device) teardown() error {
	dev := filepath.Join(d.devPath, d.scsi.VolumeName)
	tpgtPath, _ := d.getSCSIPrefixAndWnn()
	lunPath := d.getLunPath(tpgtPath)

	/*
		We're removing:
		/sys/kernel/config/target/loopback/naa.<id>/tpgt_1/lun/lun_0/<volume name>
		/sys/kernel/config/target/loopback/naa.<id>/tpgt_1/lun/lun_0
		/sys/kernel/config/target/loopback/naa.<id>/tpgt_1
		/sys/kernel/config/target/loopback/naa.<id>
		/sys/kernel/config/target/core/user_42/<volume name>
	*/
	pathsToRemove := []string{
		path.Join(lunPath, d.scsi.VolumeName),
		lunPath,
		tpgtPath,
		path.Dir(tpgtPath),
		path.Join(d.hbaDir, d.scsi.VolumeName),
	}

	for _, p := range pathsToRemove {
		if d.toClean[p] {
			err := remove(p)
			if err != nil {
				d.logger().Error("tcmu: remove failed", "err", err)
			}
		}
	}

	// Should be cleaned up automatically, but if it isn't remove it
	if _, err := os.Stat(dev); err == nil {
		if d.toClean[dev] {
			err := remove(dev)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func removeAsync(path string, done chan<- error) {
	slog.Debug("tcmu: removing", "path", path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Error("tcmu: unable to remove", "path", path)
		done <- err
		return
	}
	slog.Debug("tcmu: removed", "path", path)
	done <- nil
}

func remove(path string) error {
	done := make(chan error)
	go removeAsync(path, done)
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		return fmt.Errorf("timeout trying to delete %s", path)
	}
}
