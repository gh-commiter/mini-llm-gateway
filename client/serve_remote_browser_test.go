package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteSettingsPageModeAndCSP(t *testing.T) {
	f := remoteTransport(t)
	w := f.run(f.request("GET", "/minigw/settings", ""))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, s := range []string{`data-remote="true"`, `const PICKER = false;`, `type: "password"`, `autocomplete: "current-password"`, `minigw-remote-session`} {
		if !strings.Contains(w.Body.String(), s) {
			t.Fatal("missing", s)
		}
	}
	if strings.Contains(w.Body.String(), "{{") {
		t.Fatal("unresolved template")
	}
	for _, v := range []string{"require-trusted-types-for 'script'", "frame-ancestors 'none'", "form-action 'none'", "default-src 'none'", "script-src 'nonce-", "connect-src 'self'"} {
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), v) {
			t.Fatal("CSP missing", v)
		}
	}
	for key, want := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff"} {
		if w.Header().Get(key) != want {
			t.Fatal(key, w.Header())
		}
	}
	if strings.Contains(w.Body.String(), f.token) || strings.Contains(w.Body.String(), remoteTestPassword) {
		t.Fatal("secret in page")
	}
}

func TestRemoteSettingsBrowserLoginScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	start := strings.Index(settingsPage, "const REMOTE =")
	end := strings.Index(settingsPage, "const SL =")
	elStart := strings.Index(settingsPage, "function el(")
	elEnd := strings.Index(settingsPage, "// workRow")
	if start < 0 || end < start || elStart < 0 || elEnd < elStart {
		t.Fatal("script markers")
	}
	script := `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = SOURCE;
const make = tag => ({tag, children: [], events: {}, value: '',
 append(...kids) { this.children.push(...kids); },
 replaceChildren(...kids) { this.children = kids; },
 addEventListener(k,v) { this.events[k] = v; }, focus() {}});
async function run(remote) {
 const nav=make('nav'),page=make('page'),nodes=[];
 const store=new Map([['minigw-session','LOCAL-SECRET']]);
 const calls=[];
 let failure=remote;
 const context={console,URLSearchParams,
  document:{documentElement:{dataset:{remote:String(remote)}},createElement:tag=>{const n=make(tag);nodes.push(n);return n;},getElementById:id=>id==='nav'?nav:page},
  location:{hash:'#t=LOCAL-LINK',pathname:'/minigw/settings'},history:{replaceState(){}},
  sessionStorage:{getItem:k=>store.get(k),setItem:(k,v)=>store.set(k,v),removeItem:k=>store.delete(k)},
  show:()=>{},toast:()=>{},
  fetch:async (path,opts)=>{calls.push({path,...opts});return {ok:!failure,status:failure?429:200,json:async()=> failure ? {error:{message:'<img src=x onerror=bad()>'}} : {session:remote?'a'.repeat(64):'LOCAL-SESSION'}};},
 };
 vm.createContext(context);
 vm.runInContext(source+'\nconst SL=false;',context);
 if (!remote) {
  await vm.runInContext('signIn()',context);
  assert.equal(store.get('minigw-session'),'LOCAL-SESSION');
  assert.deepEqual(JSON.parse(calls[0].body),{t:'LOCAL-LINK'});
  assert.equal(vm.runInContext('settingsRequestHeaders()["X-Minigw-Session"]',context),'LOCAL-SESSION');
  assert.equal(calls[0].headers['X-Minigw-Remote-Request'],undefined);
  return;
 }
 const wait=vm.runInContext('signIn()',context);
 assert.equal(calls.length,0); // fragment and local storage never authenticate remotely
 const form=page.children[0],password=nodes.find(n=>n.type==='password');
 assert.equal(form.tag,'form');assert.equal(password.autocomplete,'current-password');
 password.value='password secret';
 await form.events.submit({preventDefault(){}});
 assert.equal(password.value,'');
 assert.equal(nodes.find(n=>n.role==='status').textContent,'<img src=x onerror=bad()>');
 assert.equal(calls[0].redirect,'error');assert.equal(calls[0].credentials,'omit');
 assert.equal(calls[0].headers['X-Minigw-Remote-Request'],'1');
 assert.deepEqual(JSON.parse(calls[0].body),{password:'password secret'});
 failure=false;password.value='correct password';
 await form.events.submit({preventDefault(){}});await wait;
 assert.equal(store.get('minigw-remote-session'),'a'.repeat(64));
 assert.equal(store.get('minigw-session'),'LOCAL-SECRET');
 assert.equal(vm.runInContext('settingsRequestHeaders()["X-Minigw-Remote-Session"]',context),'a'.repeat(64));
 assert.equal(vm.runInContext('settingsRequestHeaders()["X-Minigw-Session"]',context),undefined);
 await vm.runInContext('remoteLogout()',context);
 assert.equal(calls[2].path,'/minigw/settings/logout');
 assert.equal(calls[2].headers['X-Minigw-Remote-Session'],'a'.repeat(64));
 assert.equal(store.has('minigw-remote-session'),false);
}
(async()=>{await run(true);await run(false);})().catch(e=>{console.error(e);process.exitCode=1});
`
	// A JSON string literal safely embeds the existing script in the test program.
	source := strings.ReplaceAll(settingsPage[start:end], "{{PICKER}}", "false") + "\n" + settingsPage[elStart:elEnd]
	b, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	script = strings.Replace(script, "SOURCE", string(b), 1)
	file := filepath.Join(t.TempDir(), "remote-login.js")
	if err := os.WriteFile(file, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, file).CombinedOutput(); err != nil {
		t.Fatalf("login script: %v\n%s", err, out)
	}
}

func TestRemoteSettingsBrowserRestrictions(t *testing.T) {
	for _, s := range []string{`if (REMOTE) throw new Error("File dialogs are unavailable remotely")`, `if (REMOTE && !["remoteHome", "gateway", "addGateway", "devices", "remoteTeam", "remoteReview"].includes(page))`, `if (REMOTE) return;`, `REMOTE ? Promise.resolve({}) : api("GET", "/general")`, `if (REMOTE && !SESSION) return;  // the session ended while loading`, `if (!SESSION) throw e;  // signed out (401)`, `t("roProxyOld")`, `async remoteReview()`, `new TextEncoder().encode(s).length`, `replace(/\p{Cc}/gu, " ")`, `v.reviewCheck === "ok" && !v.isReplacement ? button(t("rvAccept")`, `e.status === 429 && e.retry`, `drafts.delete(v.id)`, `go.disabled = cancel.disabled = true;`, `text("rv-text", v.body || "")`, `dir: "ltr"`, `.rv-text { white-space: pre-wrap; overflow-wrap: anywhere; tab-size: 4; unicode-bidi: isolate;`, `r.setAttribute("aria-expanded", "false")`, `ev.key === "Enter"`, `t("roNotFound")`, `t("roFromTeam", o.name, o.role)`, `@media(max-width:700px)`, `.ro, .ro .label, .ro .value { overflow-wrap: anywhere; }`, `.ro .row[aria-expanded="true"]::before`, `api("GET", "/projects/" + x.id + "/members")`, `api("GET", "/teams/" + x.id + "/assignments")`, `!REMOTE ? group(row(labelSub(t("anyDevice")`} {
		if !strings.Contains(settingsPage, s) {
			t.Fatal("missing remote restriction", s)
		}
	}
}
