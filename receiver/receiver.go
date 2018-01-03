package main

import (
	"../abp"
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/rand"
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
	STATE_CLOSED0
	STATE_CLOSED1
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
	activeTimer  *time.Timer
	filename     string
	state        int
	lastData     []byte
	lastHdr      *abp.Header
	remoteAddr   *net.UDPAddr
	conn         *net.UDPConn
	writer       *bufio.Writer
	fh           *os.File
	lastOutFlags int
}

var crc32q = crc32.MakeTable(0xD5828281)

func reply(client *Client, flags int) {
	checksum := abp.Header{Length: 0, Flags: uint16(flags)}
	serializedHeader := abp.SerializeHeader(checksum)

	checksum.Checksum = crc32.Checksum(serializedHeader[:4], crc32q)
	serializedHeader = abp.SerializeHeader(checksum)

	_, err := client.conn.WriteToUDP(serializedHeader, client.remoteAddr)
	if err != nil {
		panic(err)
	}
	fmt.Printf("[NET] ACK with flags=%d sent to %v\n", flags,
		*client.remoteAddr)

	// save last flags in case we need to resend an ACK later
	client.lastOutFlags = flags

	// 10 second timeout which will mark the client as dead
	armTimeout(client, 10)
}

func armTimeout(client *Client, secs int) {
	if client.activeTimer != nil {
		client.activeTimer.Stop()
	}
	timeStr := time.Now().Format(time.StampMilli)
	client.activeTimer = time.AfterFunc(time.Second*time.Duration(secs), func() {
		fmt.Printf("[TIMER] Timeout hit for client %s (state=%d), set at %s!\n",
			client.remoteAddr, client.state, timeStr)
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

	reply(client, 0)
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

func removeClientAndDelete(client *Client) {
	removeClient(client)
	fmt.Printf("[HANDLER] deleted partially received file\n")
	os.Remove("./" + client.filename)
}

func resendAck(client *Client) {
	reply(client, client.lastOutFlags)
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

	if (client.lastHdr.Flags & abp.HDR_ALTERNATING) != 0 {
		client.state = STATE_CLOSED1
	} else {
		client.state = STATE_CLOSED0
	}

	reply(client, int(client.lastHdr.Flags))
}

func receiveData(client *Client) {
	writeData(client)
	if client.state == STATE_WAIT_DATA1 {
		// If this was ACK1 we're now expecting DATA0 next, other
		// packets will trigger an ACK1 retransmit
		client.state = STATE_WAIT_DATA0
		reply(client, abp.HDR_ALTERNATING)
	} else {
		client.state = STATE_WAIT_DATA1
		reply(client, 0)
	}
	fmt.Printf("[HANDLER] got data, new state=%d\n", client.state)
}

var fsmTable [5][7](func(*Client))

func initFsm() {
	fsmTable[STATE_WAIT_FILENAME][EVENT_DATA0] = removeClientAndDelete
	fsmTable[STATE_WAIT_FILENAME][EVENT_DATA1] = removeClientAndDelete
	fsmTable[STATE_WAIT_FILENAME][EVENT_FILENAME] = saveFilename
	fsmTable[STATE_WAIT_FILENAME][EVENT_FIN0] = removeClientAndDelete
	fsmTable[STATE_WAIT_FILENAME][EVENT_FIN1] = removeClientAndDelete
	fsmTable[STATE_WAIT_FILENAME][EVENT_TIMEOUT] = removeClientAndDelete

	fsmTable[STATE_WAIT_DATA0][EVENT_DATA0] = receiveData
	fsmTable[STATE_WAIT_DATA0][EVENT_DATA1] = resendAck
	fsmTable[STATE_WAIT_DATA0][EVENT_TIMEOUT] = removeClientAndDelete
	fsmTable[STATE_WAIT_DATA0][EVENT_FIN0] = receiveLastData
	// can't happen because we would already be in CLOSED1 if we
	// already got a FIN1 -> error:
	fsmTable[STATE_WAIT_DATA0][EVENT_FIN1] = removeClientAndDelete
	// can't happen because WAIT_FILENAME can only transition to
	// WAIT_DATA1, not WAIT_DATA0:
	fsmTable[STATE_WAIT_DATA0][EVENT_FILENAME] = removeClientAndDelete

	fsmTable[STATE_WAIT_DATA1][EVENT_DATA0] = resendAck
	fsmTable[STATE_WAIT_DATA1][EVENT_DATA1] = receiveData
	fsmTable[STATE_WAIT_DATA1][EVENT_FILENAME] = resendAck
	// can't happen because we would already be in CLOSED0 if we
	// already got a FIN0 -> error:
	fsmTable[STATE_WAIT_DATA1][EVENT_FIN0] = removeClientAndDelete
	fsmTable[STATE_WAIT_DATA1][EVENT_FIN1] = receiveLastData
	fsmTable[STATE_WAIT_DATA1][EVENT_TIMEOUT] = removeClientAndDelete

	fsmTable[STATE_CLOSED0][EVENT_DATA0] = removeClientAndDelete
	fsmTable[STATE_CLOSED0][EVENT_DATA1] = removeClientAndDelete
	fsmTable[STATE_CLOSED0][EVENT_FILENAME] = removeClientAndDelete
	fsmTable[STATE_CLOSED0][EVENT_FIN0] = resendAck
	fsmTable[STATE_CLOSED0][EVENT_FIN1] = removeClientAndDelete
	fsmTable[STATE_CLOSED0][EVENT_TIMEOUT] = removeClient

	fsmTable[STATE_CLOSED1][EVENT_DATA0] = removeClientAndDelete
	fsmTable[STATE_CLOSED1][EVENT_DATA1] = removeClientAndDelete
	fsmTable[STATE_CLOSED1][EVENT_FILENAME] = removeClientAndDelete
	fsmTable[STATE_CLOSED1][EVENT_FIN0] = removeClientAndDelete
	fsmTable[STATE_CLOSED1][EVENT_FIN1] = resendAck
	fsmTable[STATE_CLOSED1][EVENT_TIMEOUT] = removeClient
}

func fsmLookup(state int, event int) func(*Client) {
	return fsmTable[state][event]
}

func processDatagram(remoteAddr *net.UDPAddr, buffer []byte, clients map[string]*Client, conn *net.UDPConn) {
	// look if we've already got one from this remoteAddr
	if _, ok := clients[remoteAddr.String()]; ok {
		//fmt.Printf("[NET] Already seen client %s\n",
		//     remoteAddr.String())

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
		armTimeout(clients[remoteAddr.String()], 10)
		fmt.Printf("[NET] NEW client %v\n", remoteAddr)
	}
	client := clients[remoteAddr.String()]

	// XXX clean up dead clients periodically

	if !abp.VerifyChecksum(buffer) {
		fmt.Printf("[NET] checksum missmatch for %v discarding packet...\n",
			client)
		return
	}

	// parse packet; fill client struct with seperated header + payload
	var hdr abp.Header
	binary.Read(bytes.NewReader(buffer[:abp.HeaderLength]), binary.BigEndian, &hdr)
	client.lastHdr = &hdr
	client.lastData = buffer[abp.HeaderLength : uint16(abp.HeaderLength)+hdr.Length]
	client.remoteAddr = remoteAddr

	// FINs (may still contain data!)
	if hdr.Flags == abp.HDR_FIN {
		fmt.Printf("[FSM] %s (state=%d) -> GOT_FIN0\n",
			remoteAddr.String(), client.state)
		fsmLookup(client.state, EVENT_FIN0)(client)
		return
	}
	if hdr.Flags == (abp.HDR_FIN | abp.HDR_ALTERNATING) {
		fmt.Printf("[FSM] %s (state=%d) -> GOT_FIN1\n",
			remoteAddr.String(), client.state)
		fsmLookup(client.state, EVENT_FIN1)(client)
		return
	}

	// FILENAME flag set + no ACK
	if hdr.Flags == abp.HDR_FILENAME {
		fmt.Printf("[FSM] %s -> GOT_FILENAME\n", remoteAddr.String())
		fsmLookup(client.state, EVENT_FILENAME)(client)
		return
	}

	// ACKs + data
	if hdr.Flags == abp.HDR_ALTERNATING {
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

func dropDatagram(enabled bool, buffer []byte, reinject *bool) bool {
	dropProb := 0.1
	duplicateProb := 0.05
	bitFlipProb := 0.05
	ret := false

	if !enabled {
		return false
	}

	if rand.Intn(100) < int(dropProb*100) {
		fmt.Print("========== DROPPING PACKET ==============\n")
		ret = true
	}

	if rand.Intn(100) < int(duplicateProb*100) {
		fmt.Print("========== DUPLICATING PACKET ==============\n")
		*reinject = true
	}

	if rand.Intn(100) < int(bitFlipProb*100) {
		fmt.Print("========== INJECTING BIT ERROR ==============\n")
		buffer[rand.Intn(len(buffer))] ^= (1 << uint(rand.Intn(8)))
	}

	return ret
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

	enableLosses := false
	if len(os.Args) > 1 {
		fmt.Print("Enabling packet loss simulation!\n")
		enableLosses = true
	}

	fmt.Printf("Waiting for clients on 127.0.0.1:1234...\n")
	for {
		// blockingly wait for new datagrams
		_, remoteaddr, err := ser.ReadFromUDP(dgramBuffer)
		if err != nil {
			panic(err)
		}
		fmt.Printf("[NET] new message from %v\n", remoteaddr)

		// For demonstration purposes: drop some datagrams and
		// flip some bits in the payload. both things should be
		// detected and lead to re-transmits.
		reinject := false
		if !dropDatagram(enableLosses, dgramBuffer, &reinject) {
			processDatagram(remoteaddr, dgramBuffer, clients, ser)
		}
		if reinject {
			processDatagram(remoteaddr, dgramBuffer, clients, ser)
		}
	}
}
