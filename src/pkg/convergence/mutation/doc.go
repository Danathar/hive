// Package mutation stages the convergence mutation executor: the claim,
// journal, and ledger machinery that would apply a convergence decision as a
// durable, crash-safe repository mutation.
//
// It is wired into the hive binary through cmd/hive/mutationwire.go (#6064)
// as part of the convergence rollout work (#4246/#4263).
package mutation
