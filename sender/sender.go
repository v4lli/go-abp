package main

import (
	"../rdt"
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"os"
	unsafe "unsafe"
)

// takes a header structure and a variable-length data byte array, assembles
// them into one big bytearray and calculates+inserts the crc32 checksum into
// the resulting thing.
func finalizePkg(hdr rdt.Header, data []byte) []byte {
	crc32q := crc32.MakeTable(0xD5828281)
	serializedHeader := rdt.SerializeHeader(hdr)

	ret := make([]byte, len(serializedHeader)+int(hdr.Length))
	copy(ret, serializedHeader)
	copy(ret[len(serializedHeader):], data[:hdr.Length])

	chk := crc32.Checksum(ret[4:], crc32q)
	hdr.Checksum = chk

	copy(ret, rdt.SerializeHeader(hdr))
	return ret
}

func waitForAck(conn *net.UDPConn, wantFlags int) bool {
	inputBuf := make([]byte, 8)
	conn.ReadFromUDP(inputBuf)

	// parse packet into rdt.Header structure
	var replyHdr rdt.Header
	binary.Read(bytes.NewReader(inputBuf[:8]), binary.BigEndian, &replyHdr)

	if int(replyHdr.Flags) == wantFlags {
		return true
	} else {
		fmt.Printf("[NET] invalid reply; got Flags=%x, want Flags=%x...\n",
			replyHdr.Flags)
		return false
	}
}

func main() {
	// command line argument handling
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s <filename>\n", os.Args[0])
		os.Exit(1)
	}
	filename := []byte(os.Args[1])

	fh, err := os.Open(string(filename))
	if err != nil {
		panic(err)
	}
	fhReader := bufio.NewReader(fh)

	// XXX make configurable
	udpAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:1234")
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Connected to 127.0.0.1:1234! - ")

	var outHdr rdt.Header
	hdrLen := unsafe.Sizeof(outHdr)
	// payload size incl. header is set to <= 512 because of minimum MTU of 576
	// minus udp header minus IP header minus some IP header options (not
	// all 60 bytes though...)
	maxPayload := 512 - hdrLen
	fmt.Printf("hdrLen=%d, max payload len=%d\n", hdrLen, maxPayload)

	// assert filename < payload size XXX
	// first send the file name
	out := make([]byte, maxPayload)
	copy(out, filename)

	// XXX check size before cast...
	outHdr.Length = uint16(len(filename))
	outHdr.Flags = rdt.HDR_FILENAME

	// send out filename pkgs as long as we've got no ACK
	sendbuffer := finalizePkg(outHdr, out)
	for {
		_, err := conn.Write(sendbuffer)
		if err != nil {
			panic(err)
		}
		fmt.Printf("Sent FILENAME packet with %d bytes (Flags=0x%x).\n",
			len(sendbuffer), outHdr.Flags)

		if waitForAck(conn, 0) {
			break
		}
	}

	// we can now start sending data
	lastState := false
	for {
		count, readErr := fhReader.Read(out)

		outHdr.Length = uint16(count)
		outHdr.Flags = 0

		if !lastState {
			outHdr.Flags |= rdt.HDR_ALTERNATING
		}

		// if this was a short read (i.e. err == io.EOF) we
		// send all data (may also be 0), possibly the ACK
		// bit AND the fin flag.
		if readErr == io.EOF {
			outHdr.Flags |= rdt.HDR_FIN
		}

		sendbuffer = finalizePkg(outHdr, out)
		for {
			cnt, err := conn.Write(sendbuffer)
			fmt.Printf("SENT: total bytes=%d Flags=0x%x Length=%d\n",
				cnt, outHdr.Flags, outHdr.Length)
			if err != nil {
				panic(err)
			}

			// nb: if we sent Flags=ACK1|FIN, we're also expecting
			// an ACK1|FIN reply. if we sent ACK0|FIN, we're
			// expecting only FIN.
			if waitForAck(conn, int(outHdr.Flags)) {
				fmt.Printf("Received correct ACK (Flags=0x%x)\n", outHdr.Flags)
				lastState = !lastState
				break
			} else {
				fmt.Print("Received invalid ACK... resending datagram\n")
			}
		}
		if readErr == io.EOF {
			fmt.Print("FIN sent + FINACK received, terminating client.\n")
			break
		}
	}

	conn.Close()
}
