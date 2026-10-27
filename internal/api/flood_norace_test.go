//go:build !race

package api

// s2FloodN is the size of each half of the audit flood test (DETAIL §9 S2:
// 1M rejected Bearers and 1M release calls).
const s2FloodN = 1_000_000
