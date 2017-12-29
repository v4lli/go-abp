# Compile and Run

The receiver part:

```
cd receiver/
go build
./receiver
```

The client part (transmits file.jpg):

```
cd sender/
go build
cp ~/file.jpg .
./sender file.jpg
```

# Verify file after transmission
```sha256sum file.jpg```
