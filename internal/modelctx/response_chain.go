package modelctx

import "context"

type responseChainDisabledKey struct{}

func DisableResponseChain(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, responseChainDisabledKey{}, true)
}

func ResponseChainDisabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	disabled, _ := ctx.Value(responseChainDisabledKey{}).(bool)
	return disabled
}
