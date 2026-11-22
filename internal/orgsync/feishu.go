package orgsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Feishu directory client, adapted from dsh-ee
// (server/internal/feishu/client.go, Apache-2.0):
//   - POST /open-apis/auth/v3/tenant_access_token/internal
//     {app_id, app_secret} for the tenant token (cached until
//     expiry), Authorization: Bearer on contact calls.
//   - GET /open-apis/contact/v3/departments/parentchildren for
//     child departments, GET
//     /open-apis/contact/v3/users/find_by_department for members
//     (page_token pagination), GET /open-apis/contact/v3/users/:id
//     for one member; envelope {code, msg, data}.
//   - Error codes for a single lookup: 41012 open_id not found
//     (gone), 41050 outside the app's directory scope.
//
// Differences from dsh-ee: one DirectoryClient interface shared
// with the other providers, so departments/users are normalised
// into the common Directory shape (open_department_id / open_id
// as the external ids); the client never touches dsh-ee's
// identity-linking store, that lives in the Syncer here.

const (
	feishuCodeUserNotFound = 41012
	feishuCodeNoAuthority  = 41050
)

// FeishuAPIError is a Feishu API business error (HTTP 200,
// code != 0).
type FeishuAPIError struct {
	Step string
	Code int
	Msg  string
}

func (e *FeishuAPIError) Error() string {
	return fmt.Sprintf("%s: code %d %s", e.Step, e.Code, e.Msg)
}

type feishuClient struct {
	cfg     Config
	tokenMu sync.Mutex
	token   string
	expires time.Time
}

func (c *feishuClient) call(ctx context.Context, step, method, path string, body any, token string, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s: invalid request", step)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.baseURL()+path, rdr)
	if err != nil {
		return fmt.Errorf("%s: invalid request", step)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.cfg.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("%s: request failed", step)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("%s: HTTP %d", step, res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("%s: invalid response", step)
	}
	return nil
}

func (c *feishuClient) tenantToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if time.Now().Before(c.expires) {
		return c.token, nil
	}
	var out struct {
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
		Token string `json:"tenant_access_token"`
		Exp   int    `json:"expire"`
	}
	if err := c.call(ctx, "Feishu tenant token", http.MethodPost,
		"/open-apis/auth/v3/tenant_access_token/internal",
		map[string]string{"app_id": c.cfg.AppKey, "app_secret": c.cfg.AppSecret}, "", &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", &FeishuAPIError{Step: "Feishu tenant token", Code: out.Code, Msg: out.Msg}
	}
	if out.Token == "" {
		return "", fmt.Errorf("Feishu tenant token: empty token")
	}
	secs := out.Exp
	if secs <= 0 {
		secs = 7200
	}
	c.token = out.Token
	c.expires = time.Now().Add(time.Duration(secs-60) * time.Second)
	return c.token, nil
}

type feishuEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func (c *feishuClient) contactCall(ctx context.Context, step, method, path string, out any) error {
	token, err := c.tenantToken(ctx)
	if err != nil {
		return err
	}
	var env feishuEnvelope
	if err := c.call(ctx, step, method, path, nil, token, &env); err != nil {
		return err
	}
	if env.Code != 0 {
		return &FeishuAPIError{Step: step, Code: env.Code, Msg: env.Msg}
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("%s: invalid response", step)
		}
	}
	return nil
}

type feishuDepartment struct {
	ID   string `json:"open_department_id"`
	Name string `json:"name"`
}

type feishuUser struct {
	OpenID          string   `json:"open_id"`
	Name            string   `json:"name"`
	Email           string   `json:"email"`
	EnterpriseEmail string   `json:"enterprise_email"`
	EmployeeNo      string   `json:"employee_no"`
	DepartmentIDs   []string `json:"department_ids"`
	JobTitle        string   `json:"job_title"`
}

func (u feishuUser) toDirectoryUser() DirectoryUser {
	email := strings.TrimSpace(u.EnterpriseEmail)
	if email == "" {
		email = strings.TrimSpace(u.Email)
	}
	return DirectoryUser{
		ExternalID: strings.TrimSpace(u.OpenID),
		Name:       strings.TrimSpace(u.Name),
		Email:      email,
		DeptIDs:    append([]string(nil), u.DepartmentIDs...),
		Title:      strings.TrimSpace(u.JobTitle),
		JobNumber:  strings.TrimSpace(u.EmployeeNo),
	}
}

// Directory walks the whole organisation from the root department
// (open_department_id "0"): breadth-first over child departments,
// page_token-paged member lists. Any page error fails the whole
// call, so the caller never applies a partial directory.
func (c *feishuClient) Directory(ctx context.Context) (*Directory, error) {
	out := &Directory{Departments: map[string]string{"0": ""}, Users: []DirectoryUser{}}
	queue := []string{"0"}
	seenUsers := map[string]bool{}
	for next := 0; next < len(queue); next++ {
		dept := queue[next]
		pageToken := ""
		for {
			q := url.Values{
				"department_id": {dept},
				"page_size":     {"100"},
				"user_id_type":  {"open_id"},
			}
			if pageToken != "" {
				q.Set("page_token", pageToken)
			}
			var page struct {
				HasMore   bool         `json:"has_more"`
				PageToken string       `json:"page_token"`
				Items     []feishuUser `json:"items"`
			}
			if err := c.contactCall(ctx, "Feishu department members", http.MethodGet,
				"/open-apis/contact/v3/users/find_by_department?"+q.Encode(), &page); err != nil {
				return nil, err
			}
			for _, u := range page.Items {
				du := u.toDirectoryUser()
				if du.ExternalID == "" || seenUsers[du.ExternalID] {
					continue
				}
				seenUsers[du.ExternalID] = true
				out.Users = append(out.Users, du)
			}
			if !page.HasMore {
				break
			}
			if page.PageToken == "" || page.PageToken == pageToken {
				return nil, fmt.Errorf("Feishu department members: invalid pagination token for department %s", dept)
			}
			pageToken = page.PageToken
		}
		pageToken = ""
		for {
			q := url.Values{
				"department_id": {dept},
				"page_size":     {"100"},
			}
			if pageToken != "" {
				q.Set("page_token", pageToken)
			}
			var page struct {
				HasMore   bool               `json:"has_more"`
				PageToken string             `json:"page_token"`
				Items     []feishuDepartment `json:"items"`
			}
			if err := c.contactCall(ctx, "Feishu departments", http.MethodGet,
				"/open-apis/contact/v3/departments/parentchildren?"+q.Encode(), &page); err != nil {
				return nil, err
			}
			for _, d := range page.Items {
				if _, seen := out.Departments[d.ID]; d.ID == "" || seen {
					continue
				}
				out.Departments[d.ID] = d.Name
				queue = append(queue, d.ID)
			}
			if !page.HasMore {
				break
			}
			if page.PageToken == "" || page.PageToken == pageToken {
				return nil, fmt.Errorf("Feishu departments: invalid pagination token for department %s", dept)
			}
			pageToken = page.PageToken
		}
	}
	sort.Slice(out.Users, func(i, j int) bool { return out.Users[i].ExternalID < out.Users[j].ExternalID })
	return out, nil
}

// Lookup fetches one member by open_id.
func (c *feishuClient) Lookup(ctx context.Context, openID string) (*DirectoryUser, error) {
	var u feishuUser
	if err := c.contactCall(ctx, "Feishu member lookup", http.MethodGet,
		"/open-apis/contact/v3/users/"+url.PathEscape(openID)+"?user_id_type=open_id", &u); err != nil {
		return nil, err
	}
	du := u.toDirectoryUser()
	if du.ExternalID != openID || openID == "" {
		return nil, errors.New("Feishu member lookup: response did not match requested open_id")
	}
	return &du, nil
}

// UserState confirms one member; only an explicit "not found"
// (41012) counts as gone, out-of-scope (41050) does not.
func (c *feishuClient) UserState(ctx context.Context, externalID string) (UserState, error) {
	_, err := c.Lookup(ctx, externalID)
	var apiErr *FeishuAPIError
	switch {
	case err == nil:
		return UserActive, nil
	case errors.As(err, &apiErr) && apiErr.Code == feishuCodeUserNotFound:
		return UserGone, nil
	case errors.As(err, &apiErr) && apiErr.Code == feishuCodeNoAuthority:
		return UserOutOfScope, nil
	}
	return UserActive, err
}
