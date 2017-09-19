lnwallet
=========

[![Build Status](http://img.shields.io/travis/samvrlewis/lnd.svg)](https://travis-ci.org/samvrlewis/lnd) 
[![MIT licensed](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/samvrlewis/lnd/blob/master/LICENSE)
[![GoDoc](https://img.shields.io/badge/godoc-reference-blue.svg)](http://godoc.org/github.com/samvrlewis/lnd/lnwallet)

The lnwallet package implements an abstracted wallet controller that is able to
drive channel funding workflows, a number of script utilities, witness
generation functions for the various Lightning scripts, revocation key
derivation, and the commitment update state machine. 

The package is used within `lnd` as the core wallet of the daemon. The wallet
itself is composed of several distinct interfaces that decouple the
implementation of things like signing and blockchain access. This separation
allows new `WalletController` implementations to be be easily dropped into
`lnd` without disrupting the code base. A series of integration tests at the
interface level are also in place to ensure conformance of the implementation
with the interface.


## Installation and Updating

```bash
$ go get -u github.com/samvrlewis/lnd/lnwallet
```
