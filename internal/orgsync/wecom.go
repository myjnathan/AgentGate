package orgsync

import (
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

// WeCom directory client, following the official enterprise WeChat
// (企业微信) address-book API shapes
// (developer.work.weixin.qq.com, qyapi.weixin.qq.com):
//   - GET /cgi-bin/gettoken?corpid=&corpsecret= for the
//     access_token (cached until expiry).
//   - GET /cgi-bin/department/list?access_token=&id= for the flat
//     department list (parentid links the tree).
//   - GET /cgi-bin/user/list?access_token=&department_id=&
//     fetch_child=1 for the members of a department subtree.
//   - GET /cgi-bin/user/get?access_token=&userid= for one member.
//   All responses are {errcode, errmsg, ...}; errcode != 0 is a
//   business error. dsh-ee does not implement WeCom, so this client
//   is new code in the same shape as the other two.
//
// The token rides in the query string, so error messages never
// carry the request URL.

// WeCom error codes used when confirming one member.
const (
	wecomCodeUserNotFound = 60111
	wecomCodeTokenInvalid = 40014
	wecomCodeTokenExpired = 42001
	wecomCodeTokenMissing = 41001
)

// WeComAPIError is a WeCom business error (HTTP 200, errcode != 0).
type WeComAPIError struct {
	Step string
	Code int
	Msg  string
}

func (e *WeComAPIError) Error() string {
	return fmt.Sprintf("%s: errcode %d %s", e.Step, e.Code, e.Msg)
}

type wecomClient struct {
	cfg     Config
	tokenMu sync.Mutex
	token   string
	expires time.Time
}

func (c *wecomClient) get(ctx context.Context, step, path string, params url.Values, out any) error {
	q := params
	if q == nil {
		q = url.Values{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.baseURL()+path+"?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("%s: invalid request", step)
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

func (c *wecomClient) accessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if time.Now().Before(c.expires) {
		return c.token, nil
	}
	var out struct {
		Code    int    `json:"errcode"`
		Msg     string `json:"errmsg"`
		Token   string `json:"access_token"`
		Expires int    `json:"expires_in"`
	}
	q := url.Values{"corpid": {c.cfg.CorpID}, "corpsecret": {c.cfg.AppSecret}}
	if err := c.get(ctx, "WeCom access token", "/cgi-bin/gettoken", q, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", &WeComAPIError{Step: "WeCom access token", Code: out.Code, Msg: out.Msg}
	}
	if out.Token == "" {
		return "", fmt.Errorf("WeCom access token: empty token")
	}
	secs := out.Expires
	if secs <= 0 {
		secs = 7200
	}
	c.token = out.Token
	c.expires = time.Now().Add(time.Duration(secs-60) * time.Second)
	return c.token, nil
}

type wecomEnvelope struct {
	Code int    `json:"errcode"`
	Msg  string `json:"errmsg"`
}

func (c *wecomClient) apiCall(ctx context.Context, step, path string, params url.Values, out any) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	if params == nil {
		params = url.Values{}
	}
	params.Set("access_token", token)
	// Decode into the envelope first, then re-decode the full body
	// into out: decode the response twice via a captured copy.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.baseURL()+path+"?"+params.Encode(), nil)
	if err != nil {
		return fmt.Errorf("%s: invalid request", step)
	}
	res, err := c.cfg.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("%s: request failed", step)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("%s: HTTP %d", step, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: invalid response", step)
	}
	var env wecomEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("%s: invalid response", step)
	}
	if env.Code != 0 {
		return &WeComAPIError{Step: step, Code: env.Code, Msg: env.Msg}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("%s: invalid response", step)
		}
	}
	return nil
}

type wecomDepartment struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	ParentID int    `json:"parentid"`
}

type wecomUser struct {
	UserID     string `json:"userid"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Department []int  `json:"department"`
	Position   string `json:"position"`
}

func (u wecomUser) toDirectoryUser() DirectoryUser {
	var depts []string
	for _, id := range u.Department {
		depts = append(depts, strconv.Itoa(id))
	}
	return DirectoryUser{
		ExternalID: strings.TrimSpace(u.UserID),
		Name:       strings.TrimSpace(u.Name),
		Email:      strings.TrimSpace(u.Email),
		DeptIDs:    depts,
		Title:      strings.TrimSpace(u.Position),
	}
}

// Directory fetches the flat department list and the members of
// the root department subtree (fetch_child=1) in two calls. Any
// error fails the whole call, so the caller never applies a
// partial directory.
func (c *wecomClient) Directory(ctx context.Context) (*Directory, error) {
	out := &Directory{Departments: map[string]string{}, Users: []DirectoryUser{}}
	var deptResp struct {
		wecomEnvelope
		Departments []wecomDepartment `json:"department"`
	}
	if err := c.apiCall(ctx, "WeCom departments", "/cgi-bin/department/list",
		url.Values{"id": {"1"}}, &deptResp); err != nil {
		return nil, err
	}
	for _, d := range deptResp.Departments {
		out.Departments[strconv.Itoa(d.ID)] = d.Name
	}
	var userResp struct {
		wecomEnvelope
		Users []wecomUser `json:"userlist"`
	}
	if err := c.apiCall(ctx, "WeCom department members", "/cgi-bin/user/list",
		url.Values{"department_id": {"1"}, "fetch_child": {"1"}}, &userResp); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, u := range userResp.Users {
		du := u.toDirectoryUser()
		if du.ExternalID == "" || seen[du.ExternalID] {
			continue
		}
		seen[du.ExternalID] = true
		out.Users = append(out.Users, du)
	}
	sort.Slice(out.Users, func(i, j int) bool { return out.Users[i].ExternalID < out.Users[j].ExternalID })
	return out, nil
}

// Lookup fetches one member by userid.
func (c *wecomClient) Lookup(ctx context.Context, userID string) (*DirectoryUser, error) {
	var u struct {
		wecomEnvelope
		wecomUser
	}
	if err := c.apiCall(ctx, "WeCom member lookup", "/cgi-bin/user/get",
		url.Values{"userid": {userID}}, &u); err != nil {
		return nil, err
	}
	du := u.toDirectoryUser()
	if du.ExternalID != userID || userID == "" {
		return nil, errors.New("WeCom member lookup: response did not match requested userid")
	}
	return &du, nil
}

// UserState confirms one member; only an explicit "not found"
// (60111) counts as gone. Token errors are returned as errors, so
// a bad secret never disables anyone.
func (c *wecomClient) UserState(ctx context.Context, externalID string) (UserState, error) {
	_, err := c.Lookup(ctx, externalID)
	var apiErr *WeComAPIError
	switch {
	case err == nil:
		return UserActive, nil
	case errors.As(err, &apiErr) && apiErr.Code == wecomCodeUserNotFound:
		return UserGone, nil
	}
	return UserActive, err
}
