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
	"strconv"
	"strings"
	"sync"
	"time"
)

// DingTalk directory client, adapted from dsh-ee
// (server/internal/dingtalk/client.go and directory.go, Apache-2.0):
//   - POST /v1.0/oauth2/accessToken {appKey, appSecret} for the app
//     token (cached until expiry), new-style api.dingtalk.com host.
//   - POST /topapi/v2/department/listsub for child departments,
//     POST /topapi/v2/user/list (cursor/size<=100) for members,
//     POST /topapi/v2/user/get for one member; all on
//     oapi.dingtalk.com with access_token in the query and a
//     {errcode, errmsg, result} envelope.
//
// Differences from dsh-ee: the token host is derived from BaseURL
// (dsh-ee hardcodes api.dingtalk.com/oapi.dingtalk.com); department
// ids are strings so all three providers share one Directory type;
// error messages never carry the request URL (the token rides in
// the query string).

// DingTalk error codes used when confirming one member.
const (
	dingTalkCodeUserNotFound   = 60121
	dingTalkCodeUserOutOfScope = 50002
	dingTalkCodeDeptOutOfScope = 50004
	dingTalkCodeNoPermission   = 60011
)

// DingTalkAPIError is a DingTalk oapi business error (HTTP 200,
// errcode != 0).
type DingTalkAPIError struct {
	Step string
	Code int64
	Msg  string
}

func (e *DingTalkAPIError) Error() string {
	return fmt.Sprintf("%s: errcode %d %s", e.Step, e.Code, e.Msg)
}

type dingTalkClient struct {
	cfg     Config
	tokenMu sync.Mutex
	token   string
	expires time.Time
}

func (c *dingTalkClient) apiHost() string {
	// The access-token endpoint lives on api.dingtalk.com while the
	// address-book endpoints live on oapi.dingtalk.com. A BaseURL
	// override (tests / private deployment) serves both from one
	// host, so only split the hosts for the public default.
	if c.cfg.BaseURL != "" {
		return c.cfg.baseURL()
	}
	return "https://api.dingtalk.com"
}

func (c *dingTalkClient) doJSON(ctx context.Context, step, method, endpoint, token string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s: invalid request", step)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("%s: invalid request", step)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("x-acs-dingtalk-access-token", token)
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

func (c *dingTalkClient) appToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if time.Now().Before(c.expires) {
		return c.token, nil
	}
	var out struct {
		Token   string `json:"accessToken"`
		Seconds int    `json:"expireIn"`
	}
	if err := c.doJSON(ctx, "DingTalk app token", http.MethodPost,
		c.apiHost()+"/v1.0/oauth2/accessToken", "",
		map[string]string{"appKey": c.cfg.AppKey, "appSecret": c.cfg.AppSecret}, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("DingTalk app token: empty token")
	}
	secs := out.Seconds
	if secs <= 0 {
		secs = 7200
	}
	c.token = out.Token
	c.expires = time.Now().Add(time.Duration(secs-60) * time.Second)
	return c.token, nil
}

// oapiCall posts to an oapi endpoint with the app token in the
// query and decodes the {errcode, errmsg, result} envelope. The
// token is scrubbed from any error message before it is returned.
func (c *dingTalkClient) oapiCall(ctx context.Context, step, path string, in, result any) error {
	token, err := c.appToken(ctx)
	if err != nil {
		return err
	}
	var env struct {
		Code   json.Number     `json:"errcode"`
		Msg    string          `json:"errmsg"`
		Result json.RawMessage `json:"result"`
	}
	endpoint := c.cfg.baseURL() + path + "?" + url.Values{"access_token": {token}}.Encode()
	if err := c.doJSON(ctx, step, http.MethodPost, endpoint, "", in, &env); err != nil {
		return err
	}
	code, err := env.Code.Int64()
	if err != nil {
		return fmt.Errorf("%s: invalid response", step)
	}
	if code != 0 {
		msg := strings.ReplaceAll(env.Msg, token, "[redacted]")
		if r := []rune(msg); len(r) > 200 {
			msg = string(r[:200]) + "…"
		}
		return &DingTalkAPIError{Step: step, Code: code, Msg: msg}
	}
	if result != nil {
		if err := json.Unmarshal(env.Result, result); err != nil {
			return fmt.Errorf("%s: invalid response", step)
		}
	}
	return nil
}

type dingTalkUser struct {
	UserID    string  `json:"userid"`
	Name      string  `json:"name"`
	Email     string  `json:"email"`
	OrgEmail  string  `json:"org_email"`
	JobNumber string  `json:"job_number"`
	Title     string  `json:"title"`
	DeptIDs   []int64 `json:"dept_id_list"`
}

func (u dingTalkUser) toDirectoryUser() DirectoryUser {
	email := strings.TrimSpace(u.OrgEmail)
	if email == "" {
		email = strings.TrimSpace(u.Email)
	}
	var depts []string
	for _, id := range u.DeptIDs {
		depts = append(depts, strconv.FormatInt(id, 10))
	}
	return DirectoryUser{
		ExternalID: strings.TrimSpace(u.UserID),
		Name:       strings.TrimSpace(u.Name),
		Email:      email,
		DeptIDs:    depts,
		Title:      strings.TrimSpace(u.Title),
		JobNumber:  strings.TrimSpace(u.JobNumber),
	}
}

// Directory walks the whole organisation from the root department
// (dept_id=1): breadth-first over sub-departments, cursor-paged
// member lists. Any page error fails the whole call, so the caller
// never applies a partial directory (dsh-ee rule).
func (c *dingTalkClient) Directory(ctx context.Context) (*Directory, error) {
	out := &Directory{Departments: map[string]string{"1": ""}, Users: []DirectoryUser{}}
	queue := []int64{1}
	seenUsers := map[string]bool{}
	for next := 0; next < len(queue); next++ {
		dept := queue[next]
		cursor := int64(0)
		for {
			var page struct {
				HasMore bool           `json:"has_more"`
				Next    int64          `json:"next_cursor"`
				List    []dingTalkUser `json:"list"`
			}
			if err := c.oapiCall(ctx, "DingTalk department members", "/topapi/v2/user/list",
				map[string]any{"dept_id": dept, "cursor": cursor, "size": 100}, &page); err != nil {
				return nil, err
			}
			for _, u := range page.List {
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
			if page.Next <= cursor {
				return nil, fmt.Errorf("DingTalk department members: invalid pagination cursor for department %d", dept)
			}
			cursor = page.Next
		}
		var children []struct {
			ID   int64  `json:"dept_id"`
			Name string `json:"name"`
		}
		if err := c.oapiCall(ctx, "DingTalk departments", "/topapi/v2/department/listsub",
			map[string]any{"dept_id": dept}, &children); err != nil {
			return nil, err
		}
		for _, child := range children {
			key := strconv.FormatInt(child.ID, 10)
			if _, seen := out.Departments[key]; child.ID <= 0 || seen {
				continue
			}
			out.Departments[key] = child.Name
			queue = append(queue, child.ID)
		}
	}
	sort.Slice(out.Users, func(i, j int) bool { return out.Users[i].ExternalID < out.Users[j].ExternalID })
	return out, nil
}

// Lookup fetches one member by userid.
func (c *dingTalkClient) Lookup(ctx context.Context, userID string) (*DirectoryUser, error) {
	var u dingTalkUser
	if err := c.oapiCall(ctx, "DingTalk member lookup", "/topapi/v2/user/get",
		map[string]string{"userid": userID}, &u); err != nil {
		return nil, err
	}
	du := u.toDirectoryUser()
	if du.ExternalID != userID || userID == "" {
		return nil, errors.New("DingTalk member lookup: response did not match requested staff ID")
	}
	return &du, nil
}

// UserState confirms one member. Absence from a fetched directory
// is not departure: the app's directory scope may only cover part
// of the staff, so only an explicit "not found" counts as gone.
func (c *dingTalkClient) UserState(ctx context.Context, externalID string) (UserState, error) {
	_, err := c.Lookup(ctx, externalID)
	var apiErr *DingTalkAPIError
	switch {
	case err == nil:
		return UserActive, nil
	case errors.As(err, &apiErr) && apiErr.Code == dingTalkCodeUserNotFound:
		return UserGone, nil
	case errors.As(err, &apiErr) && (apiErr.Code == dingTalkCodeUserOutOfScope ||
		apiErr.Code == dingTalkCodeDeptOutOfScope || apiErr.Code == dingTalkCodeNoPermission):
		return UserOutOfScope, nil
	}
	return UserActive, err
}
