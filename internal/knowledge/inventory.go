package knowledge

import "context"

// Inventory is an opaque site change token, not a count or completeness assessment.
// Several dependency writes in one committed transaction may advance it repeatedly.
type Inventory struct {
	SchemaVersion int   `json:"schema_version"`
	Revision      int64 `json:"revision"`
}

func (s *Service) ReadInventory(ctx context.Context, a Actor, sc Scope) (Inventory, error) {
	// Match review lock ordering and serialize with all site dependency mutations.
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return Inventory{}, err
	}
	defer tx.Rollback()
	out := Inventory{SchemaVersion: 1}
	// Sites created after migration need no runtime privilege to initialize an epoch.
	out.Revision, err = inventoryRevision(ctx, tx, sc)
	if err != nil {
		return Inventory{}, err
	}
	if err = audit(ctx, tx, a, sc, sc.SiteID, out.Revision, "inventory.read"); err != nil {
		return Inventory{}, err
	}
	if err = s.finish(ctx, tx, a); err != nil {
		return Inventory{}, err
	}
	return out, nil
}
