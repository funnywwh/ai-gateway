package httpx

import "context"

// asContext narrows a context value back to context.Context.
func asContext(ctx context.Context) context.Context { return ctx }
