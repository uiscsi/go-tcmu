package tcmu

// NewTestSCSICmd constructs a SCSICmd for unit tests without kernel TCMU
// modules. It is intended for testing [SCSICmdHandler] implementations.
//
// Parameters:
//   - cdb: the SCSI command descriptor block bytes (copied internally).
//   - dataInBuf: pre-allocated byte slice used as the data buffer.
//     [SCSICmd.Write] (handler filling a response) writes into this buffer.
//     [SCSICmd.Read] (handler consuming initiator data) reads from it.
//     The caller can inspect dataInBuf after Write to see the response.
//   - senseLen: if > 0, pre-allocates [SCSICmd.Buf] as a scratch buffer
//     of this size. Use tcmuSenseBufferSize (96) for standard sense buffer size.
//
// The returned SCSICmd has a nil [Device]. Handlers must not call
// [SCSICmd.Device] methods (Sizes, GetDevConfig, etc.) on test commands —
// they will panic. This is by design: SSC tape handlers use [SCSICmd.GetCDB]
// only, not [SCSICmd.LBA] / [SCSICmd.XferLen].
//
// [SCSICmd.Write] and [SCSICmd.Read] share a cursor (offset/vecoffset).
// Do not mix Read and Write on the same cmd instance — create a fresh cmd
// for each direction.
func NewTestSCSICmd(cdb []byte, dataInBuf []byte, senseLen int) *SCSICmd {
	cdbCopy := make([]byte, len(cdb))
	copy(cdbCopy, cdb)
	cmd := &SCSICmd{
		cdb:  cdbCopy,
		vecs: [][]byte{dataInBuf},
	}
	if senseLen > 0 {
		cmd.Buf = make([]byte, senseLen)
	}
	return cmd
}
