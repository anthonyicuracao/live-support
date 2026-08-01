// settings.go — per-tenant configuration.
//
// A key/value table rather than a column per flag: settings arrive one at a
// time and a column each would mean a migration each, against tenant databases
// that are created independently and are never all on the same version.
//
// Every setting reads through a typed accessor with an explicit default, so an
// unset key and a tenant that has never been touched behave identically —
// there is no "unconfigured" state to handle at the call site.
package main

import (
	"database/sql"
	"net/http"
	"strconv"
)

// settingReadReceipts controls whether the OTHER party is told that a message
// was read.
//
// Default ON, which Ron chose deliberately: the primary use case is live
// support, where the customer is the one waiting, and knowing their message has
// actually been read is information they benefit from. The setting exists
// because an agent may reasonably not want a visitor timing their reading, and
// that is a judgement per tenant rather than one we should make for everybody.
//
// "Delivered" is not covered by this and is always reported: it says the
// message reached a device, not that a person looked at it, so it carries none
// of the same expectation.
const settingReadReceipts = "read_receipts"

func tenantSetting(db *sql.DB, key, def string) string {
	var v string
	if err := db.QueryRow(`SELECT value FROM tenant_settings WHERE key = ?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func setTenantSetting(db *sql.DB, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO tenant_settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// readReceiptsEnabled reports whether read receipts may be recorded or shared.
//
// Anything other than an explicit "off" is on. A positive check for the
// DISABLING value, so a corrupted or unexpected setting fails toward the
// documented default rather than silently turning a feature off for a tenant
// who never asked.
func readReceiptsEnabled(db *sql.DB) bool {
	return tenantSetting(db, settingReadReceipts, "on") != "off"
}

// POST /settings/read-receipts (admin, CSRF-checked): toggle for this tenant.
func (a *authApp) setReadReceipts(w http.ResponseWriter, r *http.Request) {
	info := authFrom(r.Context())
	if info == nil {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	on, err := strconv.ParseBool(r.PostFormValue("enabled"))
	if err != nil {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	value := "off"
	if on {
		value = "on"
	}
	if err := setTenantSetting(info.db, settingReadReceipts, value); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}
