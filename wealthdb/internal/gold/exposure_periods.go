package gold

import "time"

// HoldingKeyPrefix begins the key gold gives a holding no security key
// names: its source and instrument id follow it.
const HoldingKeyPrefix = "holding:"

// PeriodEnds are the ends of the periods of months months (1, 3 or 12)
// that meet [from, to]: each period's last second, or to where to comes
// first. The first period is aligned to its month, quarter or year. None
// when from is after to.
func PeriodEnds(from, to int64, months int) []int64 {
	if from > to || months < 1 {
		return nil
	}
	t := time.Unix(from, 0).UTC()
	start := time.Date(t.Year(), t.Month()-time.Month((int(t.Month())-1)%months), 1, 0, 0, 0, 0, time.UTC)
	var out []int64
	for ; start.Unix() <= to; start = start.AddDate(0, months, 0) {
		out = append(out, min(start.AddDate(0, months, 0).Unix()-1, to))
	}
	return out
}
