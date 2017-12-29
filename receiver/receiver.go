package main

import (
	"../rdt"
	"bufio"
	"bytes"
	"encoding/binary"
	//"encoding/hex"
	"fmt"
	"hash/crc32"
	"net"
	"os"
	"time"
)

type Client struct {
	activeTimer *time.Timer
	filename    string
	state       int
	lastData    []byte
	lastHdr     *rdt.Header
	remoteAddr  *net.UDPAddr
	conn        *net.UDPConn
	writer      *bufio.Writer
	fh          *os.File
}

var crc32q = crc32.MakeTable(0xD5828281)

func reply(client *Client, flags int) {
	checksum := rdt.Header{Length: 0, Flags: uint16(flags)}
	serializedHeader := rdt.SerializeHeader(checksum)

	checksum.Checksum = crc32.Checksum(serializedHeader[:4], crc32q)
	serializedHeader = rdt.SerializeHeader(checksum)

	_, err := client.conn.WriteToUDP(serializedHeader, client.remoteAddr)
	if err != nil {
		panic(err)
	}
	fmt.Printf("[NET] ACK with flags=%d sent to %v\n", flags,
		*client.remoteAddr)
	armTimeout(client, 5)

}

func armTimeout(client *Client, secs int) {
	if client.activeTimer != nil {
		client.activeTimer.Stop()
	}
	client.activeTimer = time.AfterFunc(time.Second*time.Duration(secs), func() {
		fmt.Printf("[TIMER] Timeout hit for client %v!\n", client.remoteAddr)
		client.activeTimer = nil
		fsm(client.state, rdt.REC_GOT_TIMEOUT)(client)
	})
}

func saveFilename(client *Client) {
	client.filename = string(client.lastData[:client.lastHdr.Length])
	fmt.Printf("[HANDLER] filename=%s (len=%d)\n", client.filename,
		client.lastHdr.Length)

	// XXX sanitize filename... very insecure otherwise

	var err error
	client.fh, err = os.Create("./" + client.filename)
	if err != nil {
		panic(err)
	}
	client.writer = bufio.NewWriter(client.fh)
	client.state = rdt.REC_WAIT_DATA1

	go reply(client, 0)
}

func removeClient(client *Client) {
	fmt.Printf("[HANDLER] file %s written; set client to DEAD: %v\n",
		client.filename, *client)

	if client.writer != nil {
		client.writer.Flush()
		client.writer = nil
	}
	if client.fh != nil {
		client.fh.Sync()
		client.fh = nil
	}
	if client.activeTimer != nil {
		client.activeTimer.Stop()
		client.activeTimer = nil
	}
	client.state = rdt.REC_CLIENT_DEAD
}

func resendAck(client *Client) {
	if client.state == rdt.REC_WAIT_DATA1 {
		// If we're waiting for DATA1 and get DATA0, it will
		// trigger an ACK0 retransmit
		fmt.Printf("[HANDLER] resend ACK0 %v\n", *client)
		go reply(client, 0)
	} else {
		// ... and vice versa
		fmt.Printf("[HANDLER] resend ACK1 %v\n", *client)
		go reply(client, rdt.HDR_ALTERNATING)
	}
	// This doesn't change FSM state
}

func writeData(client *Client) {
	_, err := client.writer.Write(client.lastData)
	// err is set if nn != len(client.lastData)
	if err != nil {
		panic(err)
	}
	client.writer.Flush()
	client.fh.Sync()
}

func receiveLastData(client *Client) {
	writeData(client)
	client.state = rdt.REC_CLOSED
	go reply(client, int(client.lastHdr.Flags))
}

func receiveData(client *Client) {
	writeData(client)
	if client.state == rdt.REC_WAIT_DATA1 {
		// If this was ACK1 we're now expecting DATA0 next, other
		// packets will trigger an ACK1 retransmit
		client.state = rdt.REC_WAIT_DATA0
		go reply(client, rdt.HDR_ALTERNATING)
	} else {
		client.state = rdt.REC_WAIT_DATA1
		go reply(client, 0)
	}
	fmt.Printf("[HANDLER] got data, new state=%d\n", client.state)
}

func fsm(state int, event int) func(*Client) {
	fsmTable := [10][10](func(*Client)){}
	fsmTable[rdt.REC_WAIT_FILENAME][rdt.REC_GOT_FILENAME] = saveFilename
	fsmTable[rdt.REC_WAIT_FILENAME][rdt.REC_GOT_TIMEOUT] = resendAck
	fsmTable[rdt.REC_WAIT_FILENAME][rdt.REC_GOT_FIN0] = removeClient
	fsmTable[rdt.REC_WAIT_FILENAME][rdt.REC_GOT_FIN1] = removeClient
	fsmTable[rdt.REC_WAIT_DATA0][rdt.REC_GOT_DATA0] = receiveData
	fsmTable[rdt.REC_WAIT_DATA0][rdt.REC_GOT_DATA1] = resendAck
	fsmTable[rdt.REC_WAIT_DATA0][rdt.REC_GOT_TIMEOUT] = removeClient
	fsmTable[rdt.REC_WAIT_DATA0][rdt.REC_GOT_FIN0] = receiveLastData
	fsmTable[rdt.REC_WAIT_DATA0][rdt.REC_GOT_FIN1] = resendAck
	fsmTable[rdt.REC_WAIT_DATA1][rdt.REC_GOT_DATA0] = resendAck
	fsmTable[rdt.REC_WAIT_DATA1][rdt.REC_GOT_DATA1] = receiveData
	fsmTable[rdt.REC_WAIT_DATA1][rdt.REC_GOT_TIMEOUT] = removeClient
	fsmTable[rdt.REC_WAIT_DATA1][rdt.REC_GOT_FIN0] = resendAck
	fsmTable[rdt.REC_WAIT_DATA1][rdt.REC_GOT_FIN1] = receiveLastData
	fsmTable[rdt.REC_CLOSED][rdt.REC_GOT_DATA0] = removeClient
	fsmTable[rdt.REC_CLOSED][rdt.REC_GOT_DATA1] = removeClient
	fsmTable[rdt.REC_CLOSED][rdt.REC_GOT_FIN0] = resendAck
	fsmTable[rdt.REC_CLOSED][rdt.REC_GOT_FIN1] = resendAck
	fsmTable[rdt.REC_CLOSED][rdt.REC_GOT_TIMEOUT] = removeClient
	return fsmTable[state][event]
}

func stateLookup(remoteAddr *net.UDPAddr, buffer []byte, clients map[string]*Client, conn *net.UDPConn) {
	// look if we've already got one from this remoteAddr
	if _, ok := clients[remoteAddr.String()]; ok {
		fmt.Printf("[NET] Already seen client %s\n", remoteAddr.String())

		// remove possibly dead client & retry
		if clients[remoteAddr.String()].state == rdt.REC_CLIENT_DEAD {
			fmt.Printf("[NET] client %s dead, removing\n",
				remoteAddr.String())
			delete(clients, remoteAddr.String())
			stateLookup(remoteAddr, buffer, clients, conn)
			return
		}
	} else {
		clients[remoteAddr.String()] = &Client{
			state: rdt.REC_WAIT_FILENAME,
			conn:  conn,
		}
		armTimeout(clients[remoteAddr.String()], 5)
		fmt.Printf("[NET] NEW client %v\n", remoteAddr)
	}
	client := clients[remoteAddr.String()]

	// XXX clean up dead clients periodically

	if !rdt.VerifyChecksum(buffer) {
		fmt.Printf("[NET] checksum missmatch for %v discarding packet...\n",
			client)
		return
	}

	var hdr rdt.Header
	// XXX hardcoded header length not good
	binary.Read(bytes.NewReader(buffer[:8]), binary.BigEndian, &hdr)
	client.lastHdr = &hdr
	client.lastData = buffer[8 : 8+hdr.Length]
	client.remoteAddr = remoteAddr

	// FINs (may still contain data!)
	if hdr.Flags == rdt.HDR_FIN {
		fmt.Printf("[FSM] %s (state=%d) -> GOT_FIN0\n",
			remoteAddr.String(), client.state)
		fsm(client.state, rdt.REC_GOT_FIN0)(client)
		return
	}
	if hdr.Flags == (rdt.HDR_FIN | rdt.HDR_ALTERNATING) {
		fmt.Printf("[FSM] %s (state=%d) -> GOT_FIN1\n",
			remoteAddr.String(), client.state)
		fsm(client.state, rdt.REC_GOT_FIN1)(client)
		return
	}

	// FILENAME flag set + no ACK
	if hdr.Flags == rdt.HDR_FILENAME {
		fmt.Printf("[FSM] %s -> GOT_FILENAME\n", remoteAddr.String())
		fsm(client.state, rdt.REC_GOT_FILENAME)(client)
		return
	}

	// ACKs + data
	if hdr.Flags == rdt.HDR_ALTERNATING {
		fmt.Printf("[FSM] %s (state=%d) -> rdt.REC_GOT_DATA1\n",
			remoteAddr.String(), client.state)
		fsm(client.state, rdt.REC_GOT_DATA1)(client)
		return
	}
	if hdr.Flags == 0 {
		fmt.Printf("[FSM] %s (state=%d) -> rdt.REC_GOT_DATA0\n",
			remoteAddr.String(), client.state)
		fsm(client.state, rdt.REC_GOT_DATA0)(client)
		return
	}
}

func main() {
	p := make([]byte, 512)

	clients := make(map[string]*Client)

	addr := net.UDPAddr{
		Port: 1234,
		IP:   net.ParseIP("127.0.0.1"),
	}
	ser, err := net.ListenUDP("udp", &addr)
	if err != nil {
		fmt.Printf("Socket setup error: %v\n", err)
		return
	}

	fmt.Printf("Waiting for clients on 127.0.0.1:1234...\n")
	for {
		_, remoteaddr, err := ser.ReadFromUDP(p)

		fmt.Printf("[NET] new message from %v\n", remoteaddr)
		if err != nil {
			panic(err)
		}

		stateLookup(remoteaddr, p, clients, ser)
	}
}
