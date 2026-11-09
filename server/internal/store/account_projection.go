package store

// Account pages show identities, not one account for every file and production
// result. Original refs remain authoritative for receipts and result history.
// A missing identity is never merged; a platform boundary is never crossed.
const accountIdentitySQL = `CASE WHEN trim(identity)='' THEN 'ref:'||ref ELSE 'identity:'||lower(trim(identity)) END`

const accountProjectionPrefix = `WITH account_raw AS (
 SELECT accounts.*, ` + accountLibraryStatusSQL + ` AS library_state,
 ` + accountIdentitySQL + ` AS identity_key FROM accounts
), account_facts AS (
 SELECT account_raw.*, CASE WHEN platform<>'chatgpt' AND library_state NOT IN ('used','leased')
 THEN CASE WHEN status='ok' THEN 'available'
 WHEN status IN ('banned','invalid','expired','relogin_required','needs_refresh') THEN 'invalid'
 ELSE 'pending' END ELSE library_state END AS display_state FROM account_raw
), account_ranked AS (
 SELECT account_facts.*,
 COUNT(*) OVER account_group AS source_count,
 MAX(CASE WHEN claimed=1 OR library_state='used' THEN 1 ELSE 0 END) OVER account_group AS identity_claimed,
 MAX(CASE WHEN library_state='leased' THEN 1 ELSE 0 END) OVER account_group AS identity_leased,
 ROW_NUMBER() OVER (PARTITION BY platform,identity_key ORDER BY `

const accountProjectionSuffix = `
 CASE WHEN ref LIKE 'import:%' THEN 0 ELSE 1 END,
 health_checked_at DESC,updated_at DESC,ref ASC) AS identity_rank
 FROM account_facts WINDOW account_group AS (PARTITION BY platform,identity_key)
), account_projection AS (
 SELECT *, CASE WHEN identity_claimed=1 THEN 'used' WHEN identity_leased=1 THEN 'leased'
 ELSE display_state END AS projected_library_status
 FROM account_ranked WHERE identity_rank=1
) `

const accountProjectionSQL = accountProjectionPrefix + accountProjectionSuffix

// AccountSources exposes the original metadata behind a display identity.
// Credentials and receipt ownership remain attached to each original ref.
func (s *Store) AccountSources(ref string) ([]Account, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var platform, identity string
	if err := tx.QueryRow("SELECT platform,"+accountIdentitySQL+" FROM accounts WHERE ref=?", ref).Scan(&platform, &identity); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT ref,platform,kind,identity,secret_ref,status,health_checked_at,
	 health_message,claimed,claimed_at,refresh_state,refresh_at,note,version,
	 oauth_status,join_status,liveness_status,liveness_checked_at,updated_at,`+accountLibraryStatusSQL+`
	 FROM accounts WHERE platform=? AND (`+accountIdentitySQL+`)=? ORDER BY updated_at DESC,ref`, platform, identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.Ref, &a.Platform, &a.Kind, &a.Identity, &a.SecretRef, &a.Status, &a.HealthCheckedAt,
			&a.HealthMessage, &a.Claimed, &a.ClaimedAt, &a.RefreshState, &a.RefreshAt, &a.Note, &a.Version,
			&a.OAuthStatus, &a.JoinStatus, &a.LivenessStatus, &a.LivenessCheckedAt, &a.UpdatedAt, &a.LibraryStatus); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, tx.Commit()
}

// AccountSourceRefs enumerates durable source identities for signed work. It
// must not use the display projection or its page limit: old deliveries keep
// referring to their exact source even when the page groups newer copies.
func (s *Store) AccountSourceRefs() ([]string, error) {
	rows, err := s.db.Query("SELECT ref FROM accounts ORDER BY ref")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := []string{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}
