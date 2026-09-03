module github.com/AiKeyLabs/aikey-data/query-service

go 1.26.1

require (
	// TEST-ONLY: the conversation-audit repo fence test builds its schema from the
	// real dbmigrate registry instead of a hand-rolled CREATE TABLE
	// (test-fixture-real-schema principle). conversation_records is NOT in
	// aikey-data/baseline — it ships as the v1.0.1-alpha.2 data migration. Same dep
	// collector-service already carries for the same reason.
	github.com/AiKeyLabs/aikey-config-tool v0.0.0-00010101000000-000000000000
	github.com/AiKeyLabs/aikey-data/baseline v0.0.0-00010101000000-000000000000
	github.com/AiKeyLabs/pkg/aikeycompat v0.0.0
	github.com/AiKeyLabs/pkg/aikeytime v0.0.0
	github.com/AiKeyLabs/pkg/buildinfo v0.0.0
	github.com/lib/pq v1.12.3
	modernc.org/sqlite v1.48.2
)

require (
	github.com/AiKeyLabs/pkg/mcpwire v0.0.0
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/mod v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/AiKeyLabs/pkg/buildinfo => ../../pkg/buildinfo

replace github.com/AiKeyLabs/pkg/aikeytime => ../../pkg/aikeytime

replace github.com/AiKeyLabs/pkg/mcpwire => ../../pkg/mcpwire

replace github.com/AiKeyLabs/pkg/aikeycompat => ../../pkg/aikeycompat

replace github.com/AiKeyLabs/aikey-data/baseline => ../baseline

replace github.com/AiKeyLabs/aikey-config-tool => ../../aikey-config-tool
