package pay

import "context"

// Context is context.Context, aliased so this package's interfaces read in
// their own vocabulary and an application implementing Store never imports
// context just to satisfy it.
type Context = context.Context
