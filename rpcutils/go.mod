module github.com/decred/dcrdata/rpcutils/v3

go 1.12

require (
	github.com/decred/dcrd/chaincfg/chainhash v1.0.2
	github.com/decred/dcrd/chaincfg/v2 v2.3.0
	github.com/decred/dcrd/dcrutil/v2 v2.0.1
	github.com/decred/dcrd/rpc/jsonrpc/types/v2 v2.0.0
	github.com/decred/dcrd/rpcclient/v5 v5.0.0
	github.com/decred/dcrd/wire v1.3.0
	github.com/decred/dcrdata/api/types/v5 v5.0.1
	github.com/decred/dcrdata/semver v1.0.0
	github.com/decred/dcrdata/txhelpers/v4 v4.0.1
	github.com/decred/slog v1.0.0
)

replace (
	github.com/decred/dcrd/wire v1.3.0 => ../../dcrnd/wire
)