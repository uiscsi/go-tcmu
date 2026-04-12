package scsi

import "testing"

func TestSSCOpcodes(t *testing.T) {
	// Rewind is an alias for RezeroUnit (0x01)
	if Rewind != 0x01 {
		t.Fatalf("expected Rewind=0x01, got 0x%02x", Rewind)
	}
	if Rewind != RezeroUnit {
		t.Fatalf("expected Rewind == RezeroUnit, got Rewind=0x%02x RezeroUnit=0x%02x", Rewind, RezeroUnit)
	}
	// ReportDensitySupport is 0x44
	if ReportDensitySupport != 0x44 {
		t.Fatalf("expected ReportDensitySupport=0x44, got 0x%02x", ReportDensitySupport)
	}
}

func TestSSCDeviceTypes(t *testing.T) {
	if DeviceTypeDisk != 0x00 {
		t.Fatalf("expected DeviceTypeDisk=0x00, got 0x%02x", DeviceTypeDisk)
	}
	if DeviceTypeTape != 0x01 {
		t.Fatalf("expected DeviceTypeTape=0x01, got 0x%02x", DeviceTypeTape)
	}
}

func TestSSCAsc(t *testing.T) {
	tests := []struct {
		name string
		got  uint16
		want uint16
	}{
		{"AscFilemark", AscFilemark, 0x0001},
		{"AscEarlyWarningEOM", AscEarlyWarningEOM, 0x0002},
		{"AscBeginningOfPartition", AscBeginningOfPartition, 0x0004},
		{"AscEndOfData", AscEndOfData, 0x0005},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Fatalf("%s: expected 0x%04x, got 0x%04x", tt.name, tt.want, tt.got)
		}
	}
}
