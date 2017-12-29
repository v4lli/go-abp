package rdt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// receiver states
const (
	REC_WAIT_FILENAME = iota
	REC_WAIT_DATA0
	REC_WAIT_DATA1
	REC_CLIENT_DEAD
)

// receiver events
const (
	REC_GOT_FILENAME = iota
	REC_GOT_DATA0
	REC_GOT_DATA1
	REC_GOT_FIN0
	REC_GOT_FIN1
	REC_GOT_TIMEOUT
	REC_CLOSED
)

// Header Flags
const (
	HDR_FILENAME    = 0x1
	HDR_ALTERNATING = 0x2
	HDR_FIN         = 0x4
)

type Header struct {
	Checksum uint32
	Length   uint16
	Flags    uint16
}

var HeaderLength int = 8

func SerializeHeader(hdr Header) []byte {
	var bin_buf bytes.Buffer
	binary.Write(&bin_buf, binary.BigEndian, hdr)
	return bin_buf.Bytes()
}

func VerifyChecksum(buffer []byte) bool {
	crc32q := crc32.MakeTable(0xD5828281)
	var hdr Header
	binary.Read(bytes.NewReader(buffer[:HeaderLength]), binary.BigEndian, &hdr)

	//fmt.Printf("[NET] hdr.Length=%d hdr.Flags=%d\n", hdr.Length, hdr.Flags)

	if int(hdr.Length) > len(buffer)-4 {
		fmt.Printf("VerifyChecksum: hdr.Length > len(buffer)-4 !!!\n")
		return false
	}

	calculated := crc32.Checksum(buffer[4:(hdr.Length+8)], crc32q)
	//fmt.Printf("%s\n", hex.Dump(buffer[4:(hdr.Length+8)]))
	if hdr.Checksum == calculated {
		return true
	} else {
		fmt.Printf("Missmatch %x <> %x over hdrLengt=%d buflen=%d\n",
			hdr.Checksum, calculated, hdr.Length, len(buffer))
		return false
	}
}
