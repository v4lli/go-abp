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
	"time"
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

// blockingly waits for an ACK reply, returns true if the reply's flags
// are equal to the flags supplied in wantFlags. may timeout if socket
// is configured to do so.
func waitForAck(conn *net.UDPConn, wantFlags int) bool {
	inputBuf := make([]byte, rdt.HeaderLength)
	_, _, err := conn.ReadFromUDP(inputBuf)

	if err != nil {
		if err, ok := err.(net.Error); ok && err.Timeout() {
			// this means we hit a read timeout which was previously
			// configured on conn. in that case, just return false
			// (i.e. no ack received, equivalent to bad/wrong ACK).
			fmt.Printf("[NET] hit read deadline for ACK %v\n", err)
			return false
		}
		panic(err)
	}

	// parse packet into rdt.Header structure
	var replyHdr rdt.Header
	binary.Read(bytes.NewReader(inputBuf[:rdt.HeaderLength]),
		binary.BigEndian, &replyHdr)

	if int(replyHdr.Flags) == wantFlags {
		return true
	} else {
		fmt.Printf("[NET] invalid reply; got Flags=%x, want Flags=%x...\n",
			replyHdr.Flags, wantFlags)
		return false
	}
}

func main() {
	// command line argument handling
	if len(os.Args) != 3 {
		fmt.Printf("Usage: %s <host:port> <filename>\n", os.Args[0])
		os.Exit(1)
	}
	host_port := os.Args[1]
	filename := []byte(os.Args[2])

	// open input file for reading
	fh, err := os.Open(string(filename))
	if err != nil {
		panic(err)
	}
	fhReader := bufio.NewReader(fh)

	udpAddr, _ := net.ResolveUDPAddr("udp", host_port)
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		panic(err)
	}
	// set a read timeout; this is important if our first packet gets lost;
	// all other timeouts/resends are handled by the server.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	fmt.Printf("Connected to 127.0.0.1:1234! - ")

	var outHdr rdt.Header
	// payload size incl. header is set to <= 512 because of minimum MTU of 576
	// minus udp header minus IP header minus some IP header options (not
	// all 60 bytes though...)
	maxPayload := 512 - rdt.HeaderLength
	fmt.Printf("hdrLen=%d, max payload len=%d\n", rdt.HeaderLength, maxPayload)

	// first send the file name
	out := make([]byte, maxPayload)
	fnLen := copy(out, filename)

	// cast is ok here because maxPayload will always be < UINT16_MAX
	outHdr.Length = uint16(fnLen)
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

	// this is our alternating-bit-indicator
	lastState := false
	// we can now start sending actual data
	for {
		// reads up to size of out (which is maxPayload). may also be 0!
		count, readErr := fhReader.Read(out)

		outHdr.Length = uint16(count)
		outHdr.Flags = 0

		if !lastState {
			outHdr.Flags |= rdt.HDR_ALTERNATING
		}

		// if this was a short read (i.e. err == io.EOF) we
		// send all data (may also be 0), possibly the ACK
		// bit AND the FIN flag.
		if readErr == io.EOF {
			outHdr.Flags |= rdt.HDR_FIN
		}

		sendbuffer = finalizePkg(outHdr, out)
		// actually try sending out this chunk of data.
		for {
			_, err := conn.Write(sendbuffer)
			//fmt.Printf("SENT: total bytes=%d Flags=0x%x Length=%d\n",
			//	cnt, outHdr.Flags, outHdr.Length)
			if err != nil {
				panic(err)
			}
			fmt.Print(".")

			// nb: if we sent Flags=ACK1|FIN, we're also expecting
			// an ACK1|FIN reply. if we sent ACK0|FIN, we're
			// expecting only FIN.
			if waitForAck(conn, int(outHdr.Flags)) {
				lastState = !lastState
				break
			}
		}
		if readErr == io.EOF {
			fmt.Print("\nFIN sent/FINACK received, terminating client.\n")
			break
		}
	}

	conn.Close()
}
