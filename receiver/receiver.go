package main

import (
	"../rdt"
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"net"
	"os"
	"strings"
	"time"
)

// receiver states
const (
	STATE_WAIT_FILENAME = iota
	STATE_WAIT_DATA0
	STATE_WAIT_DATA1
	STATE_CLOSED
	STATE_CLIENT_DEAD
)

// receiver events
const (
	EVENT_FILENAME = iota
	EVENT_DATA0
	EVENT_DATA1
	EVENT_FIN0
	EVENT_FIN1
	EVENT_TIMEOUT
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
		fmt.Printf("[TIMER] Timeout hit for client %s (state=%d)!\n",
			client.remoteAddr, client.state)
		client.activeTimer = nil
		fsmLookup(client.state, EVENT_TIMEOUT)(client)
	})
}

func saveFilename(client *Client) {
	client.filename = string(client.lastData[:client.lastHdr.Length])
	fmt.Printf("[HANDLER] filename=%s (len=%d)\n", client.filename,
		client.lastHdr.Length)

	// sanitize filename to prevent directory traversal
	client.filename = strings.Replace(client.filename, "/", ".", -1)
	client.filename = strings.Replace(client.filename, "\\", ".", -1)

	var err error
	client.fh, err = os.Create("./" + client.filename)
	if err != nil {
		panic(err)
	}
	client.writer = bufio.NewWriter(client.fh)
	client.state = STATE_WAIT_DATA1

	go reply(client, 0)
}

func removeClient(client *Client) {
	fmt.Printf("[HANDLER] file %s written; set client to DEAD: %v\n",
		client.filename, client.remoteAddr)

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
	client.state = STATE_CLIENT_DEAD
}

func resendAck(client *Client) {
	if client.state == STATE_WAIT_DATA1 {
		// If we're waiting for DATA1 and get DATA0, it will
		// trigger an ACK0 retransmit
		fmt.Printf("[HANDLER] resend ACK0 %v\n", client.remoteAddr)
		go reply(client, 0)
	} else {
		// ... and vice versa
		fmt.Printf("[HANDLER] resend ACK1 %v\n", client.remoteAddr)
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
	client.state = STATE_CLOSED
	go reply(client, int(client.lastHdr.Flags))
}

func receiveData(client *Client) {
	writeData(client)
	if client.state == STATE_WAIT_DATA1 {
		// If this was ACK1 we're now expecting DATA0 next, other
		// packets will trigger an ACK1 retransmit
		client.state = STATE_WAIT_DATA0
		go reply(client, rdt.HDR_ALTERNATING)
	} else {
		client.state = STATE_WAIT_DATA1
		go reply(client, 0)
	}
	fmt.Printf("[HANDLER] got data, new state=%d\n", client.state)
}

var fsmTable [5][7](func(*Client))

func initFsm() {
	fsmTable[STATE_WAIT_FILENAME][EVENT_FILENAME] = saveFilename
	fsmTable[STATE_WAIT_FILENAME][EVENT_TIMEOUT] = resendAck
	fsmTable[STATE_WAIT_FILENAME][EVENT_FIN0] = removeClient
	fsmTable[STATE_WAIT_FILENAME][EVENT_FIN1] = removeClient
	fsmTable[STATE_WAIT_DATA0][EVENT_DATA0] = receiveData
	fsmTable[STATE_WAIT_DATA0][EVENT_DATA1] = resendAck
	fsmTable[STATE_WAIT_DATA0][EVENT_TIMEOUT] = removeClient
	fsmTable[STATE_WAIT_DATA0][EVENT_FIN0] = receiveLastData
	fsmTable[STATE_WAIT_DATA0][EVENT_FIN1] = resendAck
	fsmTable[STATE_WAIT_DATA1][EVENT_DATA0] = resendAck
	fsmTable[STATE_WAIT_DATA1][EVENT_DATA1] = receiveData
	fsmTable[STATE_WAIT_DATA1][EVENT_TIMEOUT] = removeClient
	fsmTable[STATE_WAIT_DATA1][EVENT_FIN0] = resendAck
	fsmTable[STATE_WAIT_DATA1][EVENT_FIN1] = receiveLastData
	fsmTable[STATE_CLOSED][EVENT_DATA0] = removeClient
	fsmTable[STATE_CLOSED][EVENT_DATA1] = removeClient
	fsmTable[STATE_CLOSED][EVENT_FIN0] = resendAck
	fsmTable[STATE_CLOSED][EVENT_FIN1] = resendAck
	fsmTable[STATE_CLOSED][EVENT_TIMEOUT] = removeClient
}

func fsmLookup(state int, event int) func(*Client) {
	return fsmTable[state][event]
}

func processDatagram(remoteAddr *net.UDPAddr, buffer []byte, clients map[string]*Client, conn *net.UDPConn) {
	// look if we've already got one from this remoteAddr
	if _, ok := clients[remoteAddr.String()]; ok {
		fmt.Printf("[NET] Already seen client %s\n", remoteAddr.String())

		// remove possibly dead client & retry
		if clients[remoteAddr.String()].state == STATE_CLIENT_DEAD {
			fmt.Printf("[NET] client %s dead, removing\n",
				remoteAddr.String())
			delete(clients, remoteAddr.String())
			processDatagram(remoteAddr, buffer, clients, conn)
			return
		}
	} else {
		clients[remoteAddr.String()] = &Client{
			state: STATE_WAIT_FILENAME,
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

	// parse packet; fill client struct with seperated header + payload
	var hdr rdt.Header
	binary.Read(bytes.NewReader(buffer[:rdt.HeaderLength]), binary.BigEndian, &hdr)
	client.lastHdr = &hdr
	client.lastData = buffer[rdt.HeaderLength : uint16(rdt.HeaderLength)+hdr.Length]
	client.remoteAddr = remoteAddr

	// FINs (may still contain data!)
	if hdr.Flags == rdt.HDR_FIN {
		fmt.Printf("[FSM] %s (state=%d) -> GOT_FIN0\n",
			remoteAddr.String(), client.state)
		fsmLookup(client.state, EVENT_FIN0)(client)
		return
	}
	if hdr.Flags == (rdt.HDR_FIN | rdt.HDR_ALTERNATING) {
		fmt.Printf("[FSM] %s (state=%d) -> GOT_FIN1\n",
			remoteAddr.String(), client.state)
		fsmLookup(client.state, EVENT_FIN1)(client)
		return
	}

	// FILENAME flag set + no ACK
	if hdr.Flags == rdt.HDR_FILENAME {
		fmt.Printf("[FSM] %s -> GOT_FILENAME\n", remoteAddr.String())
		fsmLookup(client.state, EVENT_FILENAME)(client)
		return
	}

	// ACKs + data
	if hdr.Flags == rdt.HDR_ALTERNATING {
		fmt.Printf("[FSM] %s (state=%d) -> EVENT_DATA1\n",
			remoteAddr.String(), client.state)
		fsmLookup(client.state, EVENT_DATA1)(client)
		return
	}
	if hdr.Flags == 0 {
		fmt.Printf("[FSM] %s (state=%d) -> EVENT_DATA0\n",
			remoteAddr.String(), client.state)
		fsmLookup(client.state, EVENT_DATA0)(client)
		return
	}
}

func main() {
	initFsm()
	dgramBuffer := make([]byte, 512)
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
		// blockingly wait for new datagrams
		_, remoteaddr, err := ser.ReadFromUDP(dgramBuffer)
		if err != nil {
			panic(err)
		}
		fmt.Printf("[NET] new message from %v\n", remoteaddr)

		processDatagram(remoteaddr, dgramBuffer, clients, ser)
	}
}
