//go:build race

package api

// Under -race the flood is scaled down (the race detector is ~10x slower);
// the full 1M+1M run is the plain `go test` run.
const s2FloodN = 100_000
