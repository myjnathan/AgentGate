// Package seed creates the PoC demo company "星河精密" (Xinghe
// Precision, a fictional 120-person medical-device company) so the full
// loop can be experienced in minutes without real enterprise data.
package seed

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

// Demo credentials (documented, demo only):
//
//	admin / agentgate-admin   (platform admin)
//	lixiaofeng / demo12345    (R&D employee)
//	chenjing / demo12345      (regulatory employee)
func Demo(st *store.Store, box *secretbox.Box, selfBaseURL string) error {
	var already bool
	st.Read(func(s *store.State) { already = len(s.Users) > 0 })
	if already {
		return fmt.Errorf("store already has users; refusing to seed")
	}
	mkUser := func(username, display, dept, role, pw string) *store.User {
		h, _ := auth.HashPassword(pw)
		return &store.User{ID: store.NewID("usr"), Username: username, DisplayName: display,
			PasswordHash: h, Department: dept, Role: role}
	}
	admin := mkUser("admin", "系统管理员", "IT", "admin", "agentgate-admin")
	li := mkUser("lixiaofeng", "李晓峰", "研发部", "employee", "demo12345")
	chen := mkUser("chenjing", "陈静", "注册部", "employee", "demo12345")

	keyEnc, err := box.Encrypt("mock-upstream-key")
	if err != nil {
		return err
	}
	st.WithLock(func(s *store.State) {
		s.Users[admin.ID] = admin
		s.Users[li.ID] = li
		s.Users[chen.ID] = chen
		// Public-cloud-like channel: may receive at most internal data.
		// Trust: signed cloud (zero-retention / no-training contract).
		s.Channels["ch_cloud"] = &store.Channel{
			ID: "ch_cloud", Name: "cloud-contract (mock)", BaseURL: selfBaseURL + "/mock/v1",
			APIKeyEnc: keyEnc, KeyMasked: secretbox.Mask("mock-upstream-key"),
			Models: []string{"mock-eco", "mock-pro"}, MaxDataLevel: store.LevelInternal, Weight: 1,
			TrustType: store.TrustSignedCloud,
		}
		// Signed-cloud channel for confidential data (F17 default):
		// confidential requests route here and are desensitized by the
		// gateway first — the cloud only ever sees placeholders.
		s.Channels["ch_cloud_conf"] = &store.Channel{
			ID: "ch_cloud_conf", Name: "cloud-contract-desensitized (mock)", BaseURL: selfBaseURL + "/mock/v1",
			APIKeyEnc: keyEnc, KeyMasked: secretbox.Mask("mock-upstream-key"),
			Models: []string{"mock-pro"}, MaxDataLevel: store.LevelConfidential, Weight: 1,
			TrustType: store.TrustSignedCloud,
		}
		// Dedicated-cloud / VPC channel (F17 optional upgrade): may
		// receive up to secret data as-is — the data stays inside the
		// contracted deployment instead of a shared public cloud.
		s.Channels["ch_private"] = &store.Channel{
			ID: "ch_private", Name: "private-onprem (mock)", BaseURL: selfBaseURL + "/mock/v1",
			APIKeyEnc: keyEnc, KeyMasked: secretbox.Mask("mock-upstream-key"),
			Models: []string{"mock-pro"}, MaxDataLevel: store.LevelSecret, Weight: 1,
			TrustType: store.TrustDedicatedCloud,
		}
		// Local small-model channel (F17): the only trust type that may
		// receive top-secret data — the data never leaves the company.
		s.Channels["ch_local"] = &store.Channel{
			ID: "ch_local", Name: "local-model (mock)", BaseURL: selfBaseURL + "/mock/v1",
			APIKeyEnc: keyEnc, KeyMasked: secretbox.Mask("mock-upstream-key"),
			Models: []string{"mock-pro"}, MaxDataLevel: store.LevelSecret, Weight: 1,
			TrustType: store.TrustLocalModel,
		}
		s.KBDocs["kb_reimburse"] = &store.KBDoc{
			ID: "kb_reimburse", Title: "差旅报销制度（2026 版）", Department: "财务部",
			Level: store.LevelInternal, Version: 3,
			Content: "差旅报销制度：市内交通每日上限 150 元；跨城出差住宿一线城市上限 600 元/晚，二线 450 元/晚。报销需在行程结束后 15 个工作日内提交，附行程单与发票。部门负责人审批后由财务在 5 个工作日内打款。",
		}
		s.KBDocs["kb_register"] = &store.KBDoc{
			ID: "kb_register", Title: "二类医疗器械注册资料清单", Department: "注册部",
			Level: store.LevelConfidential, Version: 5,
			Content: "二类医疗器械首次注册资料清单：1) 产品技术要求；2) 检验报告（注册检验）；3) 临床评价资料；4) 产品说明书与标签样稿；5) 生产制造信息；6) 质量管理体系文件。本资料属受控文件，仅限注册部与研发部受管终端访问。内部项目代号：星河 XH-200 手术机器人辅助定位装置。",
		}
		s.KBDocs["kb_handbook"] = &store.KBDoc{
			ID: "kb_handbook", Title: "员工手册（公开版）", Department: "人力部",
			Level: store.LevelPublic, Version: 1,
			Content: "员工手册：标准工作时间为 9:00–18:00，午休 12:00–13:00。年假按工龄计算：满 1 年 5 天，满 10 年 10 天。公司提供午餐补贴与年度体检。",
		}
		// Demo REST tool (backlog F7): the fictional HR leave-balance
		// endpoint above, adapted into MCP tool "rest.leave_balance".
		// No credential; the demo endpoint needs none.
		s.RESTTools["rt_leave_balance"] = &store.RESTTool{
			ID: "rt_leave_balance", Name: "leave_balance",
			Description: "查询员工剩余年假天数（星河精密演示 HR 接口）",
			BaseURL:     selfBaseURL + "/mock/demo/hr", Method: "GET", Path: "/leave-balance",
			InputSchema:  []byte(`{"type":"object","properties":{"employee_id":{"type":"string"}},"required":["employee_id"]}`),
			MaxDataLevel: store.LevelInternal, MinTier: store.TierBYOA, Enabled: true,
		}
		sum := sha256.Sum256([]byte("skill-content-v1"))
		s.Skills["sk_reimburse_helper"] = &store.Skill{
			ID: "sk_reimburse_helper", Name: "reimbursement-helper", Version: "1.0.0",
			Content: "# 报销助手\n\n帮助员工按公司差旅报销制度整理报销材料，先调用 kb_search 查询最新制度再作答，并给出引用来源。",
			Scope:   "*", Status: "published", AuthorID: admin.ID, Checksum: hex.EncodeToString(sum[:]),
		}
		// Demo prompt template (backlog F8): published v1, served to
		// every department at BYOA tier, internal data at most.
		s.Prompts["pt_weekly_report"] = &store.PromptTemplate{
			ID: "pt_weekly_report", Name: "weekly_report",
			Description: "星河精密周报助手：按部门与姓名整理本周工作周报",
			Scope:       "*", MaxDataLevel: store.LevelInternal, MinTier: store.TierBYOA,
			Status: "published", CurrentVersion: 1, PublishedVersion: 1,
			Versions: []store.PromptVersion{{Version: 1,
				Content: "你是星河精密的周报助手。请为{{department}}的{{name}}整理第{{week}}周的工作周报：列出已完成事项、进行中事项与下周计划，语言简洁，用中文回答。",
				Arguments: []store.PromptArgument{
					{Name: "name", Description: "员工姓名", Required: true},
					{Name: "department", Description: "所属部门", Required: true},
					{Name: "week", Description: "周次，如 2026-W47", Required: true},
				},
				CreatedBy: admin.ID}},
			AuthorID: admin.ID,
		}
		// Demo folder knowledge-base sources (backlog F18): registered
		// only when the demo folders exist next to the working
		// directory (the repo checkout / smoke run); otherwise
		// skipped silently. The startup rescan in main indexes them.
		if abs, ok := demoFolderPath("公司制度"); ok {
			s.KBFolderSources["kbf_demo_folders"] = &store.KBFolderSource{
				ID: "kbf_demo_folders", Name: "公司制度", Path: abs,
				Level: store.LevelInternal, Enabled: true, CreatedAt: time.Now()}
		}
		if abs, ok := demoFolderPath("机密资料"); ok {
			s.KBFolderSources["kbf_demo_confidential"] = &store.KBFolderSource{
				ID: "kbf_demo_confidential", Name: "机密资料", Path: abs,
				Level: store.LevelConfidential, Enabled: true, CreatedAt: time.Now()}
		}
	})
	return st.Save()
}

// demoFolderPath resolves a demo folder under demo/folders/<name>
// relative to the working directory and returns its absolute path
// when it exists; the seed runs from many working directories (unit
// tests included), so a missing folder is a silent skip, not an error.
func demoFolderPath(name string) (string, bool) {
	rel := filepath.Join("demo", "folders", name)
	if fi, err := os.Stat(rel); err != nil || !fi.IsDir() {
		return "", false
	}
	abs, err := filepath.Abs(rel)
	if err != nil {
		return "", false
	}
	return abs, true
}

// CustomWords for the demo company: project code names treated as confidential.
var DemoCustomWords = []string{"XH-200", "星河 XH-200"}

// BannedWords placeholder list for the demo.
var DemoBannedWords = []string{"test_sensitive"}
