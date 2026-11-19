package httpapi

import (
	"agentflow/internal/auth"
	"agentflow/internal/domain"
	"agentflow/internal/notification"
	"agentflow/internal/repo"
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type larkAPIProvider struct {
	sent    int
	sendErr error
}

func (f *larkAPIProvider) Sessions(context.Context, string, string) ([]domain.BotChat, error) {
	return []domain.BotChat{{ID: "oc_group", Name: "飞书研发群", Type: "group"}}, nil
}
func (f *larkAPIProvider) Send(context.Context, string, string, string, string) error {
	f.sent++
	return f.sendErr
}

func TestNotificationBotAPIFeishuPlatform(t *testing.T) {
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.PutUser(domain.User{LoginName: "owner"}); err != nil {
		t.Fatal(err)
	}
	owner, _ := s.UserByLoginName("owner")
	if err = s.PutProject(domain.Project{ID: "lark-project", Name: "飞书项目", OwnerID: owner.ID}); err != nil {
		t.Fatal(err)
	}
	d := &deps{repo: s, authClient: auth.NewClient(auth.Config{SessionSecret: "lark-test-secret"})}
	svc := notification.New(s)
	wecom, feishu := &botAPIProvider{}, &larkAPIProvider{}
	svc.Provider, svc.Feishu = wecom, feishu
	h := &notificationHandlers{deps: d, service: svc}
	g := gin.New()
	g.POST("/bots", h.bots)
	g.PUT("/bots/:id", h.bot)
	g.GET("/bots/:id/sessions", h.sessions)
	g.POST("/bots/:id/test", h.test)
	token, err := d.authClient.SignUser(&auth.User{ID: owner.ID, LoginName: owner.LoginName, AuthTime: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: authCookieName, Value: token})
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		return rec
	}

	if rsp := request("POST", "/bots", `{"projectId":"lark-project","provider":"slack","name":"Bot","botId":"cli_a","secret":"s","enabled":true}`); rsp.Code != 400 {
		t.Fatalf("unknown platform accepted: %d %s", rsp.Code, rsp.Body.String())
	}
	rsp := request("POST", "/bots", `{"projectId":"lark-project","provider":"feishu","name":"飞书 Bot","botId":"cli_a","secret":"app-secret","enabled":true}`)
	if rsp.Code != 200 {
		t.Fatalf("create: %d %s", rsp.Code, rsp.Body.String())
	}
	var bot domain.NotificationBot
	if err = json.Unmarshal(rsp.Body.Bytes(), &bot); err != nil {
		t.Fatal(err)
	}
	if bot.Provider != domain.BotProviderFeishu {
		t.Fatalf("platform not stored: %+v", bot)
	}
	if rsp = request("PUT", "/bots/"+bot.ID, `{"provider":"wecom","name":"飞书 Bot","botId":"cli_a","enabled":true}`); rsp.Code != 400 {
		t.Fatalf("platform switch accepted: %d %s", rsp.Code, rsp.Body.String())
	}
	// Omitting the platform on update keeps it, as older clients do.
	if rsp = request("PUT", "/bots/"+bot.ID, `{"name":"飞书 Bot","botId":"cli_a","enabled":true,"targetId":"oc_group"}`); rsp.Code != 200 || !strings.Contains(rsp.Body.String(), `"provider":"feishu"`) {
		t.Fatalf("bind target: %d %s", rsp.Code, rsp.Body.String())
	}
	if rsp = request("GET", "/bots/"+bot.ID+"/sessions", ""); rsp.Code != 200 || !strings.Contains(rsp.Body.String(), "飞书研发群") {
		t.Fatalf("sessions did not route to the Feishu provider: %s", rsp.Body.String())
	}
	if rsp = request("POST", "/bots/"+bot.ID+"/test", ""); rsp.Code != 200 || feishu.sent != 1 || wecom.sent != 0 {
		t.Fatalf("test send routing: %d feishu=%d wecom=%d %s", rsp.Code, feishu.sent, wecom.sent, rsp.Body.String())
	}
	feishu.sendErr = &notification.ProviderError{Platform: domain.BotProviderFeishu, Auth: true, Message: "应用缺少权限"}
	if rsp = request("POST", "/bots/"+bot.ID+"/test", ""); rsp.Code != 502 || !strings.Contains(rsp.Body.String(), "飞书错误") {
		t.Fatalf("auth failure: %d %s", rsp.Code, rsp.Body.String())
	}
	items, _ := s.NotificationDeliveries("lark-project")
	if len(items) == 0 || items[0].Status != "awaiting_authorization" {
		t.Fatalf("auth failure status: %+v", items)
	}
	if bots, _ := s.InteractiveNotificationBots(); len(bots) != 0 {
		t.Fatalf("conversation started without being enabled: %+v", bots)
	}
	if rsp = request("PUT", "/bots/"+bot.ID, `{"name":"飞书 Bot","botId":"cli_a","enabled":true,"interactionEnabled":true}`); rsp.Code != 200 {
		t.Fatalf("enable conversation: %d %s", rsp.Code, rsp.Body.String())
	}
	if bots, _ := s.InteractiveNotificationBots(); len(bots) != 1 || bots[0].Platform() != domain.BotProviderFeishu {
		t.Fatalf("feishu bot missing from the conversation loop: %+v", bots)
	}
}

func TestBotIdentityRepliesNameThePlatform(t *testing.T) {
	_, assistant, bot := botFixture(t)
	h := &botIdentityHandlers{deps: assistant.deps, publicURL: "http://platform.example"}
	bot.Provider = domain.BotProviderFeishu
	if _, reply, _ := h.message(bot, domain.BotInteraction{SenderID: "ou_unbound", Content: "/unbind"}); !strings.Contains(reply, "当前飞书账号尚未绑定") {
		t.Fatal(reply)
	}
	bot.Provider = ""
	if _, reply, _ := h.message(bot, domain.BotInteraction{SenderID: "unbound", Content: "解除绑定"}); !strings.Contains(reply, "当前企微账号尚未绑定") {
		t.Fatal(reply)
	}
}
