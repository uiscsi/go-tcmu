# go-tcmu

Go bindings for the Linux [TCM Userspace (TCMU)](https://www.kernel.org/doc/Documentation/target/tcmu-design.txt) kernel API. Create virtual SCSI devices backed by Go handlers.

Forked from [coreos/go-tcmu](https://github.com/coreos/go-tcmu) and modernized for Go 1.25 with slog logging, context-based lifecycle, safe struct access, and tape device support.

Maintained by the [uiscsi](https://github.com/uiscsi) project.

## Requirements

- Go 1.25 or later
- Linux kernel with `target_core_user` module
- configfs mounted at `/sys/kernel/config`
- Root privileges for device creation

## Quick Start

```go
package main

import (
    "context"
    "log"

    tcmu "github.com/uiscsi/go-tcmu"
)

func main() {
    rw := /* your io.ReaderAt + io.WriterAt */

    handler := &tcmu.SCSIHandler{
        HBA:        30,
        LUN:        0,
        WWN:        tcmu.NaaWWN{OUI: "000000", VendorID: tcmu.GenerateSerial("myvol")},
        VolumeName: "myvol",
        DataSizes:  tcmu.DataSizes{VolumeSize: 5 * 1024 * 1024 * 1024, BlockSize: 512},
        DevReady:   tcmu.MultiThreadedDevReady(tcmu.ReadWriterAtCmdHandler{RW: rw}, 4),
    }

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    d, err := tcmu.OpenTCMUDevice(ctx, "/dev/myvol", handler)
    if err != nil {
        log.Fatal(err)
    }
    defer d.Close()

    // Device is now available at /dev/myvol/myvol
    select {}
}
```

## Device Type Support

The `InquiryInfo.DeviceType` field controls the SCSI peripheral device type reported in INQUIRY responses:

```go
handler := &tcmu.SCSIHandler{
    // ...
    DevReady: tcmu.SingleThreadedDevReady(tapeHandler),
}
// Set DeviceType on the InquiryInfo:
// 0x00 = block device (default, backward compatible)
// 0x01 = sequential-access (tape)
```

## External Fabric Support

When `ExternalFabric` is true, `OpenTCMUDevice` creates only the TCMU configfs backstore and UIO file descriptor -- it skips loopback fabric setup and `/dev` node creation. Use `Device.BackstorePath()` to get the configfs path for linking into an external fabric (e.g., LIO iSCSI target).

```go
handler := &tcmu.SCSIHandler{
    // ...
    ExternalFabric: true,
}

d, err := tcmu.OpenTCMUDevice(ctx, "/dev/unused", handler)
if err != nil {
    log.Fatal(err)
}

// Link the backstore into a LIO iSCSI target:
backstorePath := d.BackstorePath()
// backstorePath is e.g. "/sys/kernel/config/target/core/user_30/myvol"
```

## Kernel Setup

### configfs

Most distributions mount configfs automatically. Verify:

```sh
mount | grep configfs
# configfs on /sys/kernel/config type configfs (rw,relatime)
```

If not mounted:

```sh
sudo modprobe configfs
sudo mkdir -p /sys/kernel/config
sudo mount -t configfs none /sys/kernel/config
```

### TCMU Module

```sh
sudo modprobe target_core_user
```

## TCMU Kernel Pitfalls

Hard-won lessons from implementing and testing TCMU handlers. These are kernel-level behaviors not obvious from the TCMU design document.

### hw_block_size Must Be >= 512 for Tape

The kernel's configfs attribute `hw_block_size` rejects values below 512 with `EINVAL`. For tape devices that use variable-block mode (logical block size = 1), set `hw_block_size = 512` (the kernel's minimum) and handle variable-block semantics in the SCSI handler. The `BlockSize` in `DataSizes` controls `hw_block_size` in configfs.

### Full-Allocation Transfer Padding

The kernel always allocates `ExpectedDataTransferLen` bytes in the data-in/data-out buffers, even when the actual transfer is shorter. TCMU handlers MUST write exactly the amount of data indicated by the SCSI response, not the full buffer. The kernel copies only the handler's written bytes back to the initiator.

For READ commands that return less data than requested (e.g., variable-block tape reads), write only the actual bytes via `cmd.Write(data[:actualLen])`. The remaining buffer space is ignored.

### ILI Sense Forwarded Verbatim

Sense data returned via `cmd.RespondSenseData()` is forwarded verbatim to the SCSI initiator. The kernel does not interpret or modify the sense buffer. This means:

- The INFORMATION field (bytes 3-6 in fixed-format sense) carries the residue count as-is
- FM (filemark), EOM (end-of-medium), and ILI (incorrect length indicator) flags in byte 2 are preserved
- The handler is responsible for correct sense encoding per SPC-4 Table 27

## Custom Handlers

Implement `SCSICmdHandler` for full control over SCSI command processing:

```go
type SCSICmdHandler interface {
    HandleCommand(cmd *SCSICmd) (SCSIResponse, error)
}
```

Use `SingleThreadedDevReady` for sequential-access devices (tape) or `MultiThreadedDevReady` for random-access devices (block).

## History

Originally created by the [CoreOS](https://github.com/coreos) team. Forked to `github.com/uiscsi/go-tcmu` for continued development with Go 1.25 modernization, context-based lifecycle management, safe struct access (no unsafe.Pointer), structured logging (log/slog), tape device support (DeviceType, ExternalFabric), and test helpers (NewTestSCSICmd).

## License

Apache 2.0. See [LICENSE](LICENSE).
