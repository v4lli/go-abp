# go-abp

Go-Implementation of (a variation of?) the [Alternating Bit Protocol](https://en.wikipedia.org/wiki/Alternating_bit_protocol), with timers.

This demo implementation can be used to transmit (```sender/```)
and receive (```receiver/```) a regular file over a possibly
unreliable UDP channel. The protocol handles re-ordering (by enforcing
a strict order), bit flips in the header and payload (by calculating
checksums) and complete packet loss (sequence number).


## ABP Header

The custom header is defined as follows:

```
0             15              31
+------------------------------+
|       CRC32 Checksum         |
+--------------+---------------+
|  PL Length   |   Flags       |
+--------------+---------------+
```

* The checksum is calculated over the entire packet _except_ the first 32 bits
  (This means bits 32 to PlLength+32).
* The sequence number (aka. _alternating bit_) is implemented as a flag in the
  Flags field. Other flags are HDR_FILENAME (indicating this packet contains
  only the ASCII filename) and HDR_FIN (indicating an EOF to the receiver).
* The maximum packet size is defined to be 512 bytes incl. header
  (i.e. PlLength <= 504) to conform with a guaranteed Internet MTU of 576.

## Server (Receiver) FSM

![server fsm](https://raw.githubusercontent.com/v4lli/go-abp/master/dia/receiver.png)

## Client (Sender) FSM

![client fsm](https://raw.githubusercontent.com/v4lli/go-abp/master/dia/sender.png)

# Compile and Run

The receiver part:

```
cd receiver/ && ./test.sh
```

test.sh sets a command line option which causes the receiver to
discard and corrupt some packets randomly. This tests the robustness
of the implementation, as all injected faults (duplicated packet,
dropped packets, bit errors) should be handled by the protocol.

The client part (tests a running server process by sending a blob
file to the receiver):

```
cd sender/ && ./test.sh
```
