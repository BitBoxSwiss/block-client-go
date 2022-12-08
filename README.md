# Go blockchain client library with failover capabilities

This library currently contains:

- `jsonrpc` - a JSONRPC-2.0 library with support for subscriptions/notifications.
- `electrum` - a TCP JSON-RPC Electrum client that supports a subset of the Electrum Server RPC
  methods.
- `failover` - a generic package that lets a client connect to many servers for redundancy, with
  support for subscriptions/notifications. The primary use case at the moment is to provide failover
  support for the electrum client in case a server is down or errors in other ways, but it is
  generic enough to be useful in other contexts.

The API of this library is currenly unstable. Expect frequent breaking changes until we start
tagging versions.
