package health

// aplcorelookup.go answers one question about a converged Keycloak realm: can
// apl-core still find the client it reconciles?
//
// THE FAILURE IT ENCODES. apl-core's keycloak operator locates the `otomi` client
// with `allClients.find((el) => el.name === client.name)`, and the representation
// it builds never sets `name` — so the lookup means "the first client with no
// name". That is correct exactly while `otomi` is the only nameless client in the
// realm. Add a second one that sorts ahead of it and the operator silently
// resolves its otomi lookup to the WRONG client, then PUTs the otomi
// representation — `authorizationServicesEnabled: true` — onto it. If that client
// is public, Keycloak refuses:
//
//	Only confidential clients are allowed to set authorization settings
//
// The 500 rolls the transaction back, so NOTHING DRIFTS: `otomi` stays
// confidential, the intruder keeps working, and every client in the realm reads
// as correct. But the operator's realm reconcile aborts at that step every 30
// seconds and never reaches the stage that writes APL console users into the
// realm. The symptom surfaces three hops away — users added in the APL console
// cannot log in, and Keycloak reports `user_not_found` for accounts whose records
// are sitting in `apl-users`, read correctly by the operator's own watcher.
//
// WHY THIS IS A PREDICATE OVER THE REALM AND NOT A CHECK ON WHAT WE CREATE.
// Naming the clients llz creates (`llz`, and the smoke lane's throwaway) is the
// fix, and it is not the assertion: the property that matters is a property of the
// REALM, and anything with realm-admin can add a nameless client — an operator in
// the console, a future extension, a smoke client whose teardown failed. This
// deliberately knows nothing about who created what; it reads what apl-core will
// actually see.

import "sort"

// RealmClient is one client as Keycloak lists it. Only the two fields the lookup
// turns on are modelled; Name is "" for a client that has none, which is the whole
// subject of this file.
type RealmClient struct {
	ClientID string
	Name     string
}

// AplCoreClientID is the client apl-core reconciles, and the one that has to win
// the nameless lookup.
const AplCoreClientID = "otomi"

// AplCoreOtomiLookup decides whether apl-core's nameless lookup still resolves to
// `otomi`, returning one line per property checked.
//
// ORDER IS RECONSTRUCTED, NOT TRUSTED. apl-core takes the FIRST match in whatever
// order Keycloak's GET /clients returned, and Keycloak orders that response by
// clientId. Sorting here reproduces the operator's own view rather than depending
// on the caller having preserved it — a probe that shuffled the list would
// otherwise make this agree with nothing.
//
// FAIL-CLOSED ON VACUITY. An empty list is a failure, not a pass: this exists to
// catch a realm that looks healthy from every other angle, and "we examined no
// clients" is not evidence that the lookup resolves.
func AplCoreOtomiLookup(clients []RealmClient) (msgs []string, failed bool) {
	if len(clients) == 0 {
		return []string{"FAIL: read 0 clients from the realm — cannot vouch that apl-core's `otomi` lookup resolves"}, true
	}

	ordered := make([]RealmClient, len(clients))
	copy(ordered, clients)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ClientID < ordered[j].ClientID })

	var nameless []RealmClient
	otomiPresent := false
	for _, c := range ordered {
		if c.ClientID == AplCoreClientID {
			otomiPresent = true
		}
		if c.Name == "" {
			nameless = append(nameless, c)
		}
	}

	if !otomiPresent {
		return []string{"FAIL: realm has no `" + AplCoreClientID + "` client — apl-core has not converged this realm"}, true
	}

	if len(nameless) == 0 {
		// `otomi` itself has acquired a name. The operator's find returns nothing and
		// it falls to its `Creating otomi client` branch, which POSTs a client whose
		// id is already taken.
		return []string{
			"FAIL: every client in the realm has a name, including `" + AplCoreClientID +
				"` — apl-core's lookup returns nothing and its create branch will 409; clear the name on `" +
				AplCoreClientID + "`",
		}, true
	}

	if first := nameless[0]; first.ClientID != AplCoreClientID {
		return []string{
			"FAIL: client `" + first.ClientID + "` has no name and sorts before `" + AplCoreClientID +
				"` — apl-core's realm reconcile is resolving its `" + AplCoreClientID +
				"` lookup to it and halting on \"Only confidential clients are allowed to set authorization settings\"; " +
				"APL console users are not being written to the realm. Give `" + first.ClientID + "` a name.",
		}, true
	}

	msgs = append(msgs, "OK: apl-core's nameless lookup resolves to `"+AplCoreClientID+"`")
	if len(nameless) > 1 {
		// Not a failure: these all sort after `otomi`, so the lookup is correct today.
		// It is still worth naming them, because the next nameless client to appear
		// need only sort earlier to take the realm down the same way.
		var others []string
		for _, c := range nameless[1:] {
			others = append(others, c.ClientID)
		}
		msgs = append(msgs, "  caveat: "+joinIDs(others)+" also have no name (they sort after `"+
			AplCoreClientID+"`, so the lookup holds today) — name them before one sorts earlier")
	}
	return msgs, false
}

func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += "`" + id + "`"
	}
	return out
}
