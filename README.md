[![Build Status](https://github.com/bugst/go-serial/workflows/test/badge.svg)](https://github.com/bugst/go-serial/actions?workflow=test)

# go.bug.st/serial

A cross-platform serial port library for Go.

## Documentation and examples

See the package documentation here: https://pkg.go.dev/go.bug.st/serial

## Credits

:sparkles: Thanks to all awesome [contributors]! :sparkles:

## License

This software is released under the [BSD 3-clause license].

[contributors]: https://github.com/bugst/go-serial/graphs/contributors
[BSD 3-clause license]: https://github.com/bugst/go-serial/blob/master/LICENSE


## Regarde fork

This fork of `bugst/go-serial` (base: v1.8.0) adds `Mode.FlowControl` (`NoFlowControl`, `RTSCTSFlowControl`, `XONXOFFFlowControl`) for Linux, the BSDs and macOS (termios) and Windows (DCB flags). The zero value keeps the old behavior: no flow control. It is used by [Regarde](https://github.com/xavilach/regarde) through a `replace` directive. Tests: `flow_linux_test.go` (needs `socat`) and `flow_windows_test.go`.

Second change (found by a review of Regarde): on Unix, `Close` now wakes and waits for every pending `Read`, `Write` and `Break` before it closes the descriptor, instead of closing first. `Write` waits for room with `select` and returns `PortClosed` when the port is closed (before, a write stopped by a full queue or by flow control could not be interrupted). `Break` is cut short by `Close`. `SetDTR` and `SetRTS` are atomic with respect to each other, and the other descriptor users refuse once the port is closed, so nothing can act on a descriptor number that was reused. The descriptor is now non-blocking. Tests: `TestCloseReleasesABlockedWrite`, `TestCloseEndsABreakAndNothingTouchesTheDescriptorAfterwards`.
