package comptes

import "mytv/internal/domain"

type (
	domainFournisseur struct{ domain.Fournisseur }
	domainBaux        struct{ domain.Baux }
)

func (domainFournisseur) URLLive(string) string { return "http://f.test/live/1" }
