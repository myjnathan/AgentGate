package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestBudgetsCRUD(t *testing.T) {
	srv, _, adminSess, empSess := setupTokensAPI(t)

	call := func(method, path, sess string, body any) (int, map[string]any) {
		t.Helper()
		var buf *bytes.Buffer
		if body != nil {
			b, _ := json.Marshal(body)
			buf = bytes.NewBuffer(b)
		} else {
			buf = bytes.NewBuffer(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, buf)
		req.Header.Set("Content-Type", "application/json")
		if sess != "" {
			req.Header.Set("Authorization", "Bearer "+sess)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// The demo seed carries one budget (研发部 $50/month, 80%).
	code, listed := call("GET", "/api/budgets", adminSess, nil)
	if code != 200 {
		t.Fatalf("list budgets: got %d, want 200", code)
	}
	budgets, _ := listed["budgets"].([]any)
	if len(budgets) != 1 {
		t.Fatalf("budgets = %d, want 1 seeded", len(budgets))
	}
	seeded, _ := budgets[0].(map[string]any)
	if seeded["department"] != "研发部" || seeded["monthly_cap_usd"] != 50.0 ||
		seeded["alert_threshold"] != 0.8 || seeded["spent_usd"] != 0.0 ||
		seeded["remaining_usd"] != 50.0 || seeded["percent_used"] != 0.0 {
		t.Fatalf("seeded budget = %+v, want 研发部 $50/0.8 unspent", seeded)
	}
	seededID, _ := seeded["id"].(string)

	// Employee may not list budgets.
	if code, _ := call("GET", "/api/budgets", empSess, nil); code != 403 {
		t.Fatalf("employee list budgets: got %d, want 403", code)
	}

	// Create: happy path, then the validation matrix.
	code, created := call("POST", "/api/budgets", adminSess,
		map[string]any{"department": "注册部", "monthly_cap_usd": 100, "alert_threshold": 0.8})
	if code != 201 {
		t.Fatalf("create budget: got %d, want 201", code)
	}
	budgetID, _ := created["id"].(string)
	if budgetID == "" {
		t.Fatal("create budget returned no id")
	}
	for name, body := range map[string]map[string]any{
		"duplicate department": {"department": "研发部", "monthly_cap_usd": 10},
		"non-positive cap":     {"department": "财务部", "monthly_cap_usd": -5},
		"zero cap":             {"department": "财务部", "monthly_cap_usd": 0},
		"threshold above one":  {"department": "财务部", "monthly_cap_usd": 10, "alert_threshold": 1.5},
		"empty department":     {"department": "", "monthly_cap_usd": 10},
	} {
		want := 400
		if name == "duplicate department" {
			want = 409
		}
		if code, _ := call("POST", "/api/budgets", adminSess, body); code != want {
			t.Fatalf("%s: got %d, want %d", name, code, want)
		}
	}
	if code, _ := call("POST", "/api/budgets", empSess,
		map[string]any{"department": "财务部", "monthly_cap_usd": 10}); code != 403 {
		t.Fatalf("employee create budget: got %d, want 403", code)
	}

	// Patch: cap and threshold, then the error cases.
	code, upd := call("PATCH", "/api/budgets/"+budgetID, adminSess,
		map[string]any{"monthly_cap_usd": 200})
	if code != 200 || upd["monthly_cap_usd"] != 200.0 {
		t.Fatalf("patch cap: got %d %+v, want 200 cap=200", code, upd)
	}
	code, upd = call("PATCH", "/api/budgets/"+budgetID, adminSess,
		map[string]any{"alert_threshold": 0})
	if code != 200 || upd["alert_threshold"] != 0.0 {
		t.Fatalf("patch threshold off: got %d %+v, want 200 threshold=0", code, upd)
	}
	if code, _ := call("PATCH", "/api/budgets/"+budgetID, adminSess,
		map[string]any{"monthly_cap_usd": -1}); code != 400 {
		t.Fatalf("patch negative cap: got %d, want 400", code)
	}
	if code, _ := call("PATCH", "/api/budgets/"+budgetID, adminSess,
		map[string]any{"alert_threshold": 2}); code != 400 {
		t.Fatalf("patch threshold >1: got %d, want 400", code)
	}
	if code, _ := call("PATCH", "/api/budgets/bud_missing", adminSess,
		map[string]any{"monthly_cap_usd": 10}); code != 404 {
		t.Fatalf("patch unknown budget: got %d, want 404", code)
	}
	if code, _ := call("PATCH", "/api/budgets/"+budgetID, adminSess,
		map[string]any{}); code != 400 {
		t.Fatalf("patch nothing: got %d, want 400", code)
	}

	// Delete: the new budget goes, the seeded one stays.
	if code, _ := call("DELETE", "/api/budgets/"+budgetID, empSess, nil); code != 403 {
		t.Fatalf("employee delete budget: got %d, want 403", code)
	}
	code, _ = call("DELETE", "/api/budgets/"+budgetID, adminSess, nil)
	if code != 200 {
		t.Fatalf("delete budget: got %d, want 200", code)
	}
	if code, _ := call("DELETE", "/api/budgets/"+budgetID, adminSess, nil); code != 404 {
		t.Fatalf("delete twice: got %d, want 404", code)
	}
	_, listed = call("GET", "/api/budgets", adminSess, nil)
	budgets, _ = listed["budgets"].([]any)
	if len(budgets) != 1 || budgets[0].(map[string]any)["id"] != seededID {
		t.Fatalf("budgets after delete = %v, want only the seeded one", listed["budgets"])
	}
}
