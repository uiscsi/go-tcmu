package tcmu

import (
	"testing"

	"github.com/uiscsi/go-tcmu/scsi"
)

func TestNewTestSCSICmd(t *testing.T) {
	t.Run("Command and GetCDB", func(t *testing.T) {
		cdb := []byte{0x12, 0x00, 0x00, 0x00, 0x24, 0x00} // INQUIRY, alloc=36
		buf := make([]byte, 36)
		cmd := NewTestSCSICmd(cdb, buf, 0)

		if cmd.Command() != 0x12 {
			t.Fatalf("Command() = 0x%02x, want 0x12", cmd.Command())
		}
		if cmd.GetCDB(4) != 0x24 {
			t.Fatalf("GetCDB(4) = 0x%02x, want 0x24", cmd.GetCDB(4))
		}
	})

	t.Run("CDB is copied", func(t *testing.T) {
		cdb := []byte{0x08, 0x00, 0x00, 0x01, 0x00, 0x00}
		cmd := NewTestSCSICmd(cdb, make([]byte, 256), 0)
		cdb[0] = 0xFF // mutate original
		if cmd.Command() != 0x08 {
			t.Fatal("CDB was not copied — mutation of original affected cmd")
		}
	})

	t.Run("Write fills dataInBuf", func(t *testing.T) {
		buf := make([]byte, 8)
		cmd := NewTestSCSICmd([]byte{0x12, 0, 0, 0, 8, 0}, buf, 0)
		data := []byte{0xAA, 0xBB, 0xCC, 0xDD}
		n, err := cmd.Write(data)
		if err != nil {
			t.Fatalf("Write error: %v", err)
		}
		if n != 4 {
			t.Fatalf("Write n = %d, want 4", n)
		}
		if buf[0] != 0xAA || buf[1] != 0xBB || buf[2] != 0xCC || buf[3] != 0xDD {
			t.Fatalf("dataInBuf = %v, want [0xAA, 0xBB, 0xCC, 0xDD, ...]", buf[:4])
		}
	})

	t.Run("Read reads from dataInBuf", func(t *testing.T) {
		buf := []byte{0x01, 0x02, 0x03, 0x04}
		cmd := NewTestSCSICmd([]byte{0x15, 0x10, 0, 0, 4, 0}, buf, 0)
		out := make([]byte, 4)
		n, err := cmd.Read(out)
		if err != nil {
			t.Fatalf("Read error: %v", err)
		}
		if n != 4 {
			t.Fatalf("Read n = %d, want 4", n)
		}
		if out[0] != 0x01 || out[1] != 0x02 || out[2] != 0x03 || out[3] != 0x04 {
			t.Fatalf("Read data = %v, want [1,2,3,4]", out)
		}
	})

	t.Run("Device returns nil", func(t *testing.T) {
		cmd := NewTestSCSICmd([]byte{0x00}, make([]byte, 1), 0)
		if cmd.Device() != nil {
			t.Fatal("Device() should return nil for test commands")
		}
	})

	t.Run("Ok response", func(t *testing.T) {
		cmd := NewTestSCSICmd([]byte{0x00}, make([]byte, 1), 0)
		resp := cmd.Ok()
		if resp.status != scsi.SamStatGood {
			t.Fatalf("Ok().status = 0x%02x, want 0x%02x", resp.status, scsi.SamStatGood)
		}
	})

	t.Run("CheckCondition response", func(t *testing.T) {
		cmd := NewTestSCSICmd([]byte{0x00}, make([]byte, 1), 0)
		resp := cmd.CheckCondition(scsi.SenseIllegalRequest, scsi.AscInvalidFieldInCdb)
		if resp.status != scsi.SamStatCheckCondition {
			t.Fatalf("CheckCondition status = 0x%02x, want 0x%02x", resp.status, scsi.SamStatCheckCondition)
		}
		if resp.senseBuffer == nil {
			t.Fatal("CheckCondition sense buffer is nil")
		}
		if resp.senseBuffer[2] != scsi.SenseIllegalRequest {
			t.Fatalf("sense key = 0x%02x, want 0x%02x", resp.senseBuffer[2], scsi.SenseIllegalRequest)
		}
	})

	t.Run("RespondSenseData", func(t *testing.T) {
		cmd := NewTestSCSICmd([]byte{0x00}, make([]byte, 1), 0)
		sense := make([]byte, 18)
		sense[0] = 0x70
		sense[2] = scsi.SenseUnitAttention
		resp := cmd.RespondSenseData(scsi.SamStatCheckCondition, sense)
		if resp.status != scsi.SamStatCheckCondition {
			t.Fatalf("status = 0x%02x, want 0x%02x", resp.status, scsi.SamStatCheckCondition)
		}
		if resp.senseBuffer[2] != scsi.SenseUnitAttention {
			t.Fatalf("sense key = 0x%02x, want 0x%02x", resp.senseBuffer[2], scsi.SenseUnitAttention)
		}
	})

	t.Run("senseLen allocates Buf", func(t *testing.T) {
		cmd := NewTestSCSICmd([]byte{0x00}, make([]byte, 1), 96)
		if cmd.Buf == nil {
			t.Fatal("cmd.Buf should be allocated when senseLen > 0")
		}
		if len(cmd.Buf) != 96 {
			t.Fatalf("len(cmd.Buf) = %d, want 96", len(cmd.Buf))
		}
	})

	t.Run("senseLen zero leaves Buf nil", func(t *testing.T) {
		cmd := NewTestSCSICmd([]byte{0x00}, make([]byte, 1), 0)
		if cmd.Buf != nil {
			t.Fatal("cmd.Buf should be nil when senseLen == 0")
		}
	})
}
