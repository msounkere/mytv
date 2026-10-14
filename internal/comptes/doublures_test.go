package comptes

import "mytv/internal/domain"

type (
	domainFournisseur struct{ domain.Fournisseur }
	domainBaux        struct{ domain.Baux }
)
