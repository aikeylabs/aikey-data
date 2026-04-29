module github.com/AiKeyLabs/aikey-data/collector-service

go 1.26.1

require (
	github.com/AiKeyLabs/aikey-config-tool v0.0.0-00010101000000-000000000000
	github.com/AiKeyLabs/pkg/aikeycompat v0.0.0
	github.com/AiKeyLabs/pkg/aikeytime v0.0.0
	github.com/AiKeyLabs/pkg/buildinfo v0.0.0
	github.com/lib/pq v1.12.3
	modernc.org/sqlite v1.48.2
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.42.0 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/AiKeyLabs/aikey-config-tool => ../../aikey-config-tool

replace github.com/AiKeyLabs/pkg/buildinfo => ../../pkg/buildinfo

replace github.com/AiKeyLabs/pkg/aikeytime => ../../pkg/aikeytime

replace github.com/AiKeyLabs/pkg/aikeycompat => ../../pkg/aikeycompat
