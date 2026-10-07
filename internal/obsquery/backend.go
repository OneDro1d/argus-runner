package obsquery

import (
	"time"

	"github.com/OneDro1d/argus-runner/internal/failcontext"
)

// Backend (AC-D13) is the query surface get_sagas / get_tail_logs need from ANY log source: the
// saga timeline and the windowed, correlation-scoped log lines for one correlation id, both
// best-effort (a source that is unreachable or times out reports Available:false with a Note —
// the "observability unavailable" outcome — never a silent empty result and never a panic).
//
// *Loki has satisfied this surface all along; *BetterStack is the second implementation
// (AC-D13, an argus-config selects one — see config.Config.UseBetterStack). Introduced here
// rather than earlier because Loki was, until now, the only backend that existed.
type Backend interface {
	Sagas(correlationID, window string, anchor time.Time) failcontext.Saga
	Logs(correlationID, window string, anchor time.Time) failcontext.Logs
}

var _ Backend = (*Loki)(nil)
var _ Backend = (*BetterStack)(nil)
