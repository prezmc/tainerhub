package gateway

import "time"

// SetCertlessGrace shortens how long connections without a client
// certificate may stay open. Call it before Server or Handler.
func SetCertlessGrace(g *Gateway, d time.Duration) { g.certlessGrace = d }
