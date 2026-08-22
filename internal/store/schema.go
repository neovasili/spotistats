package store

// Attribute and index names. They are constants rather than string literals so the
// declarative Schema below, the test harness that creates tables from it, and the CDK
// stack that provisions production all agree by construction.
const (
	AttrPK     = "PK"
	AttrSK     = "SK"
	AttrGSI1PK = "GSI1PK"
	AttrGSI1SK = "GSI1SK"

	// IndexGSI1 answers "every play of one track, chronologically".
	IndexGSI1 = "GSI1"
)

// Projected attributes on GSI1. Reading ANY attribute not in this list from a GSI1 query
// yields a zero value rather than an error, which is a classic silent bug -- there is a
// test asserting exactly that behaviour so nobody discovers it in production.
var GSI1ProjectedAttributes = []string{"msPlayed", "source", "msEstimated", "trackId"}

// IndexSchema declares a global secondary index.
type IndexSchema struct {
	Name         string
	PartitionKey string
	SortKey      string
	// Projected lists the non-key attributes copied into the index. Empty means
	// KEYS_ONLY.
	Projected []string
}

// TableSchema declares the table shape.
type TableSchema struct {
	PartitionKey string
	SortKey      string
	Indexes      []IndexSchema
}

// Schema is the single source of truth for the table shape.
//
// GSI1 is NOT sparse: every play row sets GSI1PK, so the index is a complete replica of
// the play data. That is the right trade (it is the only way to answer access pattern 6
// without a scan) and it costs roughly one extra write unit per play, but calling it
// sparse would mislead whoever next tunes cost.
var Schema = TableSchema{
	PartitionKey: AttrPK,
	SortKey:      AttrSK,
	Indexes: []IndexSchema{
		{
			Name:         IndexGSI1,
			PartitionKey: AttrGSI1PK,
			SortKey:      AttrGSI1SK,
			Projected:    GSI1ProjectedAttributes,
		},
	},
}
