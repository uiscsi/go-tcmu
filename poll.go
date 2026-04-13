package tcmu

import (
	"context"
	"fmt"

	"github.com/uiscsi/go-tcmu/scsi"
	"golang.org/x/sys/unix"
)

const (
	tcmuSenseBufferSize = 96
)

func (d *Device) beginPoll(ctx context.Context) {
	defer d.wg.Done()
	defer close(d.cmdChan)

	events := make([]unix.EpollEvent, 2)
	for {
		n, err := unix.EpollWait(d.epollFd, events, -1)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			d.logger().ErrorContext(ctx, "tcmu: epoll wait error", "err", err)
			return
		}

		cancelSeen := false
		for i := range n {
			if int(events[i].Fd) == d.cancelFd {
				// Drain the eventfd.
				var buf [8]byte
				unix.Read(d.cancelFd, buf[:]) //nolint:errcheck
				cancelSeen = true
				continue
			}
			// UIO fd readable — drain commands.
			for {
				cmd, err := d.getNextCommand()
				if err != nil {
					d.logger().ErrorContext(ctx, "tcmu: get next command failed", "err", err)
					break
				}
				if cmd == nil {
					break
				}
				d.cmdChan <- cmd
			}
		}
		if cancelSeen {
			return
		}
	}
}

func (d *Device) recvResponse(ctx context.Context) {
	defer d.wg.Done()

	buf := make([]byte, 4)
	for resp := range d.respChan {
		d.completeCommand(resp)
		// Notify kernel of completed command.
		n, err := unix.Write(d.uioFd, buf)
		if n == -1 && err != nil {
			d.logger().ErrorContext(ctx, "tcmu: poll write error", "err", err)
			// Drain remaining responses so handler goroutines are not blocked.
			for range d.respChan {
			}
			return
		}
	}
}

func (d *Device) completeCommand(resp SCSIResponse) {
	off := resp.entryOff
	d.setEntRespSCSIStatus(off, resp.status)
	if resp.status != scsi.SamStatGood {
		d.copyEntRespSenseData(off, resp.senseBuffer)
	}
	// Advance kernel-visible tail past this entry now that the response is written.
	d.mbSetTail((d.mbCmdTail() + resp.entryLen) % d.mbCmdrSize())
}

func (d *Device) getNextCommand() (*SCSICmd, error) {
	for d.nextEntryOff() != d.headEntryOff() {
		off := d.nextEntryOff()
		if d.entHdrOp(off) == tcmuOpPad {
			d.cmdTail = (d.cmdTail + uint32(d.entHdrGetLen(off))) % d.mbCmdrSize()
			d.mbSetTail(d.cmdTail)
		} else if d.entHdrOp(off) == tcmuOpCmd {
			entLen := uint32(d.entHdrGetLen(off))
			out := &SCSICmd{
				id:       d.entCmdId(off),
				device:   d,
				entryOff: off,
				entryLen: entLen,
			}
			out.cdb = d.entCdb(off)
			vecs := int(d.entReqIovCnt(off))
			out.vecs = make([][]byte, vecs)
			for i := 0; i < vecs; i++ {
				v := d.entIovecN(off, i)
				out.vecs[i] = v
			}
			// Advance local read pointer but do NOT update kernel-visible tail yet.
			// The tail is advanced in completeCommand after the response is written.
			d.cmdTail = (d.cmdTail + entLen) % d.mbCmdrSize()
			return out, nil
		} else {
			panic(fmt.Sprintf("unsupported command from tcmu? %d", d.entHdrOp(off)))
		}
	}
	return nil, nil
}

func (d *Device) nextEntryOff() int {
	return int(d.cmdTail + d.mbCmdrOffset())
}

func (d *Device) headEntryOff() int {
	return int(d.mbCmdHead() + d.mbCmdrOffset())
}
