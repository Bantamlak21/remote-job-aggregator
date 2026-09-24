// Package market names the two lists the job board keeps apart: jobs in
// Ethiopia (Ethiopian employers and Ethiopia-based postings, shown in the
// Ethiopian category) and jobs from companies hiring worldwide (the main
// page). The market belongs to a job's target (the source it was collected
// through), not to its company, so one company can appear in both: its
// Greenhouse remote roles are worldwide, its Ethiojobs posting is Ethiopian.
package market

// Market is one of the two lists.
type Market string

const (
	// Ethiopia is jobs located in Ethiopia or posted by Ethiopian companies.
	Ethiopia Market = "ethiopia"
	// Worldwide is jobs from companies hiring across borders (remote-first
	// companies and remote job boards). It is the default.
	Worldwide Market = "worldwide"
)

// Valid reports whether m is one of the two markets.
func (m Market) Valid() bool { return m == Ethiopia || m == Worldwide }

// Provider is an optional interface for a collector that knows which market
// its jobs belong to. A collector that does not implement it leaves the
// market of its targets to the database default (worldwide).
type Provider interface {
	Market() Market
}
