// Sandboxed kick execution: sandbox wiring setters, per-agent sandbox
// gating, the sandbox kick runner, and sandbox audit emission.
package agent

import (
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/kubejob"
	"github.com/hivecommons/hive/pkg/sandbox"
)

func (m *Manager) setSandboxJobLauncherFactoryForTest(f func(config.SandboxJobConfig) sandbox.Launcher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxJobLauncherFactory = f
}

// jobLauncherLocked returns the launcher for a sandbox.runtime: job kick:
// the injected factory's, or a real in-cluster kubejob.Launcher.
func (m *Manager) jobLauncherLocked(job config.SandboxJobConfig) sandbox.Launcher {
	if m.sandboxJobLauncherFactory != nil {
		return m.sandboxJobLauncherFactory(job)
	}
	return &kubejob.Launcher{Options: JobOptionsFromConfig(job)}
}

// JobOptionsFromConfig converts the operator's job block into the launcher's
// options. The config package deliberately owns its own types so it does not
// import the launcher; this is the one place the two shapes meet.
func JobOptionsFromConfig(job config.SandboxJobConfig) kubejob.Options {
	opts := kubejob.Options{
		WorkspaceClaim:      job.WorkspaceClaim,
		WorkspaceClaimMount: job.WorkspaceClaimMount,
		NodeSelector:        job.NodeSelector,
		ServiceAccount:      job.ServiceAccount,
		EnvFromSecrets:      append([]string(nil), job.EnvFromSecrets...),
		TTLSeconds:          job.TTLSeconds,
		Resources: kubejob.Resources{
			Limits:   job.Resources.Limits,
			Requests: job.Resources.Requests,
		},
	}
	for _, t := range job.Tolerations {
		opts.Tolerations = append(opts.Tolerations, kubejob.Toleration{Key: t.Key, Operator: t.Operator, Value: t.Value, Effect: t.Effect})
	}
	for _, v := range job.Volumes {
		opts.ExtraVolumes = append(opts.ExtraVolumes, kubejob.VolumeMount{Name: v.Name, Claim: v.Claim, MountPath: v.MountPath, ReadOnly: v.ReadOnly})
	}
	return opts
}

func (m *Manager) SetSandboxMutationBoundary(boundary effects.Boundary) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxMutation = boundary
}
