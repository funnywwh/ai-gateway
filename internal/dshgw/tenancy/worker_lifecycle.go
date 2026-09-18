package tenancy

import (
	"context"
	"fmt"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// StopWorkerProcess destroys the worker namespace without changing the durable
// suspension intent or invoking mount hooks. Browser mount teardown uses this
// AFTER excluding its mounts, so it cannot recurse into Manager.StopWorker.
func (m *Manager) StopWorkerProcess(ctx context.Context, t registry.Tenant) error {
	return m.workers().Stop(ctx, t)
}

// ShutdownWorkers is terminal for this Manager's runner. It shares the same
// synchronized lazy initialization as start/restart and prevents subsequent
// admin or startup operations from creating namespaces during gateway teardown.
func (m *Manager) ShutdownWorkers(ctx context.Context) error {
	return m.workers().Shutdown(ctx)
}

// workerStartAllowed runs inside the runner's lifecycle gate. Consult the live
// registry, not the snapshot held by a queued browser mount restart: an admin
// may have suspended or deleted the tenant while that restart waited for the gate.
func (m *Manager) workerStartAllowed(t registry.Tenant) error {
	if m.Registry == nil {
		return nil
	}
	current, ok := m.Registry.Get(t.Name)
	if !ok {
		return fmt.Errorf("worker tenant %s no longer exists", t.Name)
	}
	if current.Suspended {
		return fmt.Errorf("worker for tenant %s is suspended", t.Name)
	}
	return nil
}
