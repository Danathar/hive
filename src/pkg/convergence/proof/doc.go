// Package proof stages convergence proof objects: the assumptions a
// convergence decision rested on, their verification, and the invalidation
// rules that retire a proof when its assumptions stop holding.
//
// It is wired into the hive binary alongside the mutation executor
// (cmd/hive/mutationwire.go, #6064) as part of the convergence rollout work
// (#4246/#4263).
package proof
