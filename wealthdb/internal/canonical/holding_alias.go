package canonical

// HoldingAlias says that a source keys one holding two ways, and which
// key the gains reports read both under (config `holding_aliases`): a
// statement's overview line and a daily feed's ISIN for one holding,
// say. AccountExternalID "" means every account of the source.
type HoldingAlias struct {
	SilverSourceID    string
	AccountExternalID string
	FromKey, ToKey    string
}
