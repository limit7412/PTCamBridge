package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/limit7412/PTCamBridge/internal/source"
)

// 設定画面が候補欄を作る部分は JavaScript にあります。ここはそれを、画面から
// 切り出したまま node で走らせて確かめます。
//
// 移してこないのは、これがブラウザ側の判断だからです。ユーザーが解像度を打つ
// たびにサーバへ訊きに行くわけにはいきません。写して 2 つ持つと、ずれた方だけが
// 直り、画面は古いままになります。
//
// node が無ければ飛ばします。Go のツールチェーンだけを持つ人の手元で、この
// パッケージ全体が落ちる理由にはできません。CI の runner は node を持っています。
func runSettingsScript(t *testing.T, body string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed, skipping the settings page script test")
	}

	// 画面から切り出す範囲。候補欄を作る一続きの部分です。見つからなければ
	// 黙って通さず失敗させます。名前が変わったのに何も試さないテストは、
	// 通っていることのほうが害になります。
	const from = "let modesFor = null;"
	const to = "\nel(\"uvc-device\").addEventListener"
	start := strings.Index(uiSettingsHTML, from)
	end := strings.Index(uiSettingsHTML, to)
	if start < 0 || end < start {
		t.Fatalf("could not find the mode helpers in the settings page (start=%d end=%d)", start, end)
	}
	script := uiSettingsHTML[start:end]

	path := filepath.Join(t.TempDir(), "modes.mjs")
	if err := os.WriteFile(path, []byte(script+"\n"+body), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	return string(out)
}

// フレームレートの候補は、選んでいる解像度に合うモードからだけ取らなければ
// なりません。両者を独立に並べると、1920x1080@5 と 640x480@30 しか持たない
// カメラに対して画面が 1920x1080@30 を選ばせます。それはまさに、この機能が
// 説明しようとしている "Could not set video options" です。
func TestSettingsPagePairsFramerateWithTheChosenSize(t *testing.T) {
	harness := `
cameraModes = [
  {format: "mjpeg", min_size: "1920x1080", max_size: "1920x1080", min_fps: 5, max_fps: 5},
  {format: "mjpeg", min_size: "640x480", max_size: "640x480", min_fps: 30, max_fps: 30},
];
let chosen = "";
const el = () => ({ value: chosen });
const listed = {};
const options = (id, values) => { listed[id] = values; };

chosen = "1920x1080";
refreshModeChoices();
const big = listed["camera-framerates"];
chosen = "640x480";
refreshModeChoices();
const small = listed["camera-framerates"];
console.log(JSON.stringify({sizes: listed["camera-sizes"], big, small}));
`
	var got struct {
		Sizes []string `json:"sizes"`
		Big   []string `json:"big"`
		Small []string `json:"small"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if want := []string{"1920x1080", "640x480"}; !equalStrings(got.Sizes, want) {
		t.Errorf("sizes = %v, want %v", got.Sizes, want)
	}
	if want := []string{"5"}; !equalStrings(got.Big, want) {
		t.Errorf("framerates for 1920x1080 = %v, want %v", got.Big, want)
	}
	if want := []string{"30"}; !equalStrings(got.Small, want) {
		t.Errorf("framerates for 640x480 = %v, want %v", got.Small, want)
	}
}

// 候補に出すフレームレートは設定に書ける値だけです。source.uvc.framerate は
// 整数なので、29.97 を勧めても入力欄が弾くか、書けても設定として通りません。
// 幅のあるモードなら間の整数が使えます。
func TestSettingsPageOffersOnlyFrameratesTheSettingCanHold(t *testing.T) {
	harness := `
let chosen = "";
const el = () => ({ value: chosen });
const listed = {};
const options = (id, values) => { listed[id] = values; };

const rates = (mode) => { cameraModes = [mode]; refreshModeChoices(); return listed["camera-framerates"]; };
console.log(JSON.stringify({
  exact: rates({min_size: "640x480", max_size: "640x480", min_fps: 30, max_fps: 30}),
  fractional: rates({min_size: "640x480", max_size: "640x480", min_fps: 29.97, max_fps: 29.97}),
  ranged: rates({min_size: "160x120", max_size: "1280x720", min_fps: 5, max_fps: 29.97}),
}));
`
	var got struct {
		Exact      []string `json:"exact"`
		Fractional []string `json:"fractional"`
		Ranged     []string `json:"ranged"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if want := []string{"30"}; !equalStrings(got.Exact, want) {
		t.Errorf("framerates of a 30fps mode = %v, want %v", got.Exact, want)
	}
	if len(got.Fractional) != 0 {
		t.Errorf("framerates of a 29.97fps-only mode = %v, want none the setting could hold", got.Fractional)
	}
	if want := []string{"29", "5"}; !equalStrings(got.Ranged, want) {
		t.Errorf("framerates of a 5-29.97fps mode = %v, want %v", got.Ranged, want)
	}
}

// 幅で答えるカメラは、両端の間のどの大きさも受け付けます。両端だけを見ると、
// 自分で打った 640x480 が候補から外れて、フレームレートが 1 つも出なくなります。
func TestSettingsPageAcceptsSizesInsideARange(t *testing.T) {
	harness := `
cameraModes = [{min_size: "160x120", max_size: "1280x720", min_fps: 5, max_fps: 30}];
let chosen = "640x480";
const el = () => ({ value: chosen });
const listed = {};
const options = (id, values) => { listed[id] = values; };

refreshModeChoices();
const inside = listed["camera-framerates"];
chosen = "1920x1080";
refreshModeChoices();
console.log(JSON.stringify({inside, outside: listed["camera-framerates"]}));
`
	var got struct {
		Inside  []string `json:"inside"`
		Outside []string `json:"outside"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if want := []string{"30", "5"}; !equalStrings(got.Inside, want) {
		t.Errorf("framerates for a size inside the range = %v, want %v", got.Inside, want)
	}
	if len(got.Outside) != 0 {
		t.Errorf("framerates for a size outside the range = %v, want none", got.Outside)
	}
}

// 調べられなかった名前を覚えてはいけません。覚えると、ffmpeg を後から入れた人や
// カメラを解放した人が、同じ名前のままではもう一度試せなくなります — 画面を
// 読み直すまで、候補は空のままです。
func TestSettingsPageLooksAgainAfterAFailedLookup(t *testing.T) {
	harness := `
const TEXT = {modesUnknown: "unknown", modesFound: "found"};
const nodes = {"uvc-device": {value: "Bigeye"}, "uvc-size": {value: ""}, "camera-modes": {textContent: ""}};
const el = (id) => nodes[id] || (nodes[id] = {value: "", textContent: ""});
const listed = {};
const options = (id, values) => { listed[id] = values; };

// 1 回目は調べられない。2 回目は直っている。
const answers = [
  {device: "Bigeye", modes: [], error: "the camera is in use"},
  {device: "Bigeye", modes: [{min_size: "640x480", max_size: "640x480", min_fps: 30, max_fps: 30}]},
];
let asked = 0;
globalThis.fetch = async () => {
  const body = answers[Math.min(asked++, answers.length - 1)];
  return { ok: true, json: async () => body };
};

await loadCameraModes();
const first = { asked, rates: listed["camera-framerates"], said: nodes["camera-modes"].textContent };
await loadCameraModes();
const second = { asked, rates: listed["camera-framerates"] };
// 成功した後は、同じ名前でもう一度訊きに行かない。
await loadCameraModes();
console.log(JSON.stringify({first, second, finally: asked}));
`
	var got struct {
		First struct {
			Asked int      `json:"asked"`
			Rates []string `json:"rates"`
			Said  string   `json:"said"`
		} `json:"first"`
		Second struct {
			Asked int      `json:"asked"`
			Rates []string `json:"rates"`
		} `json:"second"`
		Finally int `json:"finally"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.First.Asked != 1 {
		t.Fatalf("asked %d times for the first lookup, want 1", got.First.Asked)
	}
	if !strings.Contains(got.First.Said, "the camera is in use") {
		t.Errorf("the page said %q, want it to pass on why the lookup failed", got.First.Said)
	}
	if got.Second.Asked != 2 {
		t.Errorf("asked %d times after a failed lookup, want it to try the same name again", got.Second.Asked)
	}
	if want := []string{"30"}; !equalStrings(got.Second.Rates, want) {
		t.Errorf("framerates after the lookup recovered = %v, want %v", got.Second.Rates, want)
	}
	if got.Finally != 2 {
		t.Errorf("asked %d times in total, want the answer it already has to be reused", got.Finally)
	}
}

// 画面の説明と Go 側の Mode.String は同じ形でなければなりません。片方だけを
// 読んだ人が、もう片方を見て別のカメラの話だと思わないためです。
func TestSettingsPageDescribesModesTheSameWayGoDoes(t *testing.T) {
	modes := []source.Mode{
		{Format: "mjpeg", MinSize: "640x480", MaxSize: "640x480", MinFPS: 30, MaxFPS: 30},
		{Format: "yuyv422", MinSize: "160x120", MaxSize: "1280x720", MinFPS: 5, MaxFPS: 29.97},
		{MinSize: "320x240", MaxSize: "320x240"},
	}
	encoded, err := json.Marshal(modes)
	if err != nil {
		t.Fatalf("encode modes: %v", err)
	}

	out := runSettingsScript(t, "console.log(JSON.stringify(("+string(encoded)+").map(describeMode)));")
	var got []string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if len(got) != len(modes) {
		t.Fatalf("described %d modes, want %d", len(got), len(modes))
	}
	for i, m := range modes {
		if got[i] != m.String() {
			t.Errorf("the settings page describes %v as %q, Go says %q", m, got[i], m.String())
		}
	}
}

// 設定を読み込んだ直後にも候補を取りに行かなければなりません。
//
// 既にカメラ名が設定にある人は、入力欄に触りません。触らなければ change も blur も
// 起きないので、その人 — 画面を開く理由が最もある人 — だけが候補を見られない、
// ということになります。
func TestSettingsPageAsksForModesOfTheCameraAlreadyConfigured(t *testing.T) {
	if !strings.Contains(uiSettingsHTML, "load().then(loadCameraModes)") {
		t.Error("the settings page must look up the modes after it fills the form; the events it listens for do not fire when the form fills itself")
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
