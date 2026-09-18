// Gateway owns authenticated reverse HTTP transport and FUSE lifecycle.
// Tenant-side activation intentionally has no Node queue or transport listener.
export const name = 'browser-workspace'
export const inject = []
export function apply(ctx) {
  ctx.logger?.info?.('browser-workspace: browser filesystem client enabled')
}
export default { name, inject, apply }
