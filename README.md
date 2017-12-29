Go-Implementation of (a variation of?) the [Alternating Bit Protocol](https://en.wikipedia.org/wiki/Alternating_bit_protocol), with timers.

[here goes the finite state machine diagram]

# Compile and Run

The receiver part:

```
cd receiver/ && ./test.sh
```

The client part (tests a running server process by sending a blob file to the receiver):

```
cd sender/ && ./test.sh
```
