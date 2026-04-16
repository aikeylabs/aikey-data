module github.com/AiKeyLabs/aikey-data/collector-service

go 1.26.1

require (
	github.com/AiKeyLabs/pkg/buildinfo v0.0.0
	github.com/lib/pq v1.12.0
)

replace github.com/AiKeyLabs/pkg/buildinfo => ../../pkg/buildinfo
