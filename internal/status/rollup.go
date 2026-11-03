package status

import "github.com/DanielHouston/sutats/internal/model"

// A combined row (a page's whole system, or one of its groups) needs the
// time anything in it was wrong. The members' day totals cannot say: two
// components down an hour each may have been down the same hour or two
// different ones. So the sampler records each combined row as a key of its
// own, whose status at every sample is the worst of its members'.

const rollupPrefix = "@rollup/"

// SystemKey is the history key of a page's whole-system row.
func SystemKey(p model.Page) string { return rollupPrefix + p.Slug }

// GroupKey is the history key of one group's row on a page.
func GroupKey(p model.Page, group string) string { return rollupPrefix + p.Slug + "/group/" + group }

// Rollups returns a stand-in component for every combined row, on the
// default page and on each of pages, for the sampler to record beside the
// real ones. comps must already carry their incidents (ApplyIncidents), as
// Build's do.
func Rollups(comps []model.Component, pages []model.Page) []model.Component {
	var out []model.Component
	for _, p := range append([]model.Page{{}}, pages...) {
		system := model.Component{ID: SystemKey(p), Status: model.StatusUnknown}
		groups := map[string]int{}
		var rows []model.Component
		for _, c := range comps {
			if !c.OnPage(p) {
				continue
			}
			system.Status = model.Worst(max(system.Status, model.StatusOperational), c.Status)
			gi, ok := groups[c.Group]
			if !ok {
				gi = len(rows)
				groups[c.Group] = gi
				rows = append(rows, model.Component{ID: GroupKey(p, c.Group), Status: model.StatusOperational})
			}
			rows[gi].Status = model.Worst(rows[gi].Status, c.Status)
		}
		if len(rows) == 0 {
			continue
		}
		out = append(out, system)
		out = append(out, rows...)
	}
	return out
}
