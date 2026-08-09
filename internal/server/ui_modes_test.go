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
		// CI では飛ばしません。飛ばせるようにしておくと、runner の中身が変わった日に
		// この画面のテストが黙って消え、それに気づく機会が二度と来ません。
		if os.Getenv("CI") != "" {
			t.Fatal("node is required to test the settings page script")
		}
		t.Skip("node is not installed, skipping the settings page script test")
	}

	// 画面から切り出す範囲。候補欄を作る一続きの部分です。見つからなければ
	// 黙って通さず失敗させます。名前が変わったのに何も試さないテストは、
	// 通っていることのほうが害になります。
	const from = "function cameraChoices("
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
  narrow: rates({min_size: "640x480", max_size: "640x480", min_fps: 29.5, max_fps: 29.7}),
}));
`
	var got struct {
		Exact      []string `json:"exact"`
		Fractional []string `json:"fractional"`
		Ranged     []string `json:"ranged"`
		Narrow     []string `json:"narrow"`
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
	// 幅があっても、その間に整数があるとは限らない。切り下げも切り上げも外へ出る。
	if len(got.Narrow) != 0 {
		t.Errorf("framerates of a 29.5-29.7fps mode = %v, want none — neither end rounds into the range", got.Narrow)
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
const TEXT = {modesUnknown: "unknown", modesFound: "found", modesLooking: "looking"};
globalThis.document = { querySelector: () => ({ value: "uvc" }) };
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

// modesHarness は、loadCameraModes を踏むための土台。応答は release() を呼ぶまで
// 返らないので、問い合わせの最中の状態を見られる。
const modesHarness = `
const TEXT = {modesUnknown: "unknown", modesFound: "found", modesLooking: "looking"};
let sourceType = "uvc";
globalThis.document = { querySelector: () => ({ value: sourceType }) };
const nodes = {"uvc-device": {value: "A"}, "uvc-size": {value: ""}, "camera-modes": {textContent: ""}};
const el = (id) => nodes[id] || (nodes[id] = {value: "", textContent: ""});
const listed = {"camera-sizes": [], "camera-framerates": []};
const options = (id, values) => { listed[id] = values; };

// 待っている問い合わせは全部ためる。1 つしか覚えないと、重複を確かめるテストで
// 2 本目だけが解けて、1 本目が永遠に待つ (テストは落ちるが、理由が読めない)。
let pending = [];
const release = () => { const waiting = pending; pending = []; for (const resolve of waiting) resolve(); };
let asked = 0;
globalThis.fetch = async (url) => {
  asked++;
  // 訊かれたカメラについて答える。応答が返る頃の入力欄を見て答えると、答えが
  // 勝手に「今のカメラのもの」になり、古さの判定を試せなくなる。
  const device = decodeURIComponent(String(url).split("device=")[1]);
  const size = device === "A" ? "640x480" : "1280x720";
  await new Promise((resolve) => { pending.push(resolve); });
  return { ok: true, json: async () => ({
    device,
    modes: [{min_size: size, max_size: size, min_fps: 30, max_fps: 30}],
  }) };
};
`

// 新しいカメラを調べ始めたら、前のカメラの候補は消さなければなりません。
//
// 列挙には 15 秒かかることがあります。その間ずっと前のカメラの解像度が候補に
// 残っていると、ユーザーはそれを選んで保存でき、今のカメラが持っていない
// 組み合わせが設定に入ります。
func TestSettingsPageDropsTheOldCandidatesWhileItLooksUpTheNewCamera(t *testing.T) {
	harness := modesHarness + `
const first = loadCameraModes();
release();
await first;
const afterA = listed["camera-sizes"];

nodes["uvc-device"].value = "B";
const second = loadCameraModes();
const duringB = listed["camera-sizes"];
release();
await second;
console.log(JSON.stringify({afterA, duringB, afterB: listed["camera-sizes"]}));
`
	var got struct {
		AfterA  []string `json:"afterA"`
		DuringB []string `json:"duringB"`
		AfterB  []string `json:"afterB"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if want := []string{"640x480"}; !equalStrings(got.AfterA, want) {
		t.Fatalf("sizes after looking up A = %v, want %v", got.AfterA, want)
	}
	if len(got.DuringB) != 0 {
		t.Errorf("sizes while looking up B = %v, want A's candidates gone", got.DuringB)
	}
	if want := []string{"1280x720"}; !equalStrings(got.AfterB, want) {
		t.Errorf("sizes after looking up B = %v, want %v", got.AfterB, want)
	}
}

// 同じカメラの問い合わせを重ねてはいけません。
//
// 入力欄から離れると change と blur の両方が起きます。失敗した名前は覚えないので、
// 進行中の印が無いと 2 本目がその判定をすり抜け、同じカメラへ 2 本の ffmpeg が
// 同時に走ります。排他的なデバイスなので、その 2 本は互いを失敗させ得ます。
func TestSettingsPageDoesNotAskTwiceForTheSameCameraAtOnce(t *testing.T) {
	harness := modesHarness + `
// change と blur が同じ一手で入ってくる。
const both = [loadCameraModes(), loadCameraModes()];
const asking = asked;
release();
await Promise.all(both);
console.log(JSON.stringify({asking, total: asked}));
`
	var got struct {
		Asking int `json:"asking"`
		Total  int `json:"total"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.Asking != 1 {
		t.Errorf("started %d lookups for one camera, want 1", got.Asking)
	}
	if got.Total != 1 {
		t.Errorf("ran %d lookups in total, want 1", got.Total)
	}
}

// 進行中の印を下ろすのは、その要求自身だけです。
//
// A を待っている間に B を打てば B も走り出します。そこで先に返ってきた A が印を
// 無条件に消すと、B の欄で change と blur がもう一度来たときに 2 本目が通り、
// 排他的なカメラへ 2 本の ffmpeg が向かいます。
func TestSettingsPageKeepsTheLoadingMarkOfTheRequestStillRunning(t *testing.T) {
	harness := modesHarness + `
// A を走らせたまま B を始める。
const a = loadCameraModes();
nodes["uvc-device"].value = "B";
const b = loadCameraModes();
const started = asked;

// A だけを返す。B はまだ走っている。
const waitingForB = pending.slice(1);
pending = pending.slice(0, 1);
release();
await a;
pending = waitingForB;

// ここで B の欄がもう一度 change/blur を起こす。
const again = loadCameraModes();
const afterA = asked;
release();
await Promise.all([b, again]);
console.log(JSON.stringify({started, afterA}));
`
	var got struct {
		Started int `json:"started"`
		AfterA  int `json:"afterA"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.Started != 2 {
		t.Fatalf("started %d lookups for two different cameras, want 2", got.Started)
	}
	if got.AfterA != 2 {
		t.Errorf("started %d lookups in total, want the one still running to keep its mark", got.AfterA)
	}
}

// 前のカメラへ戻ったら、候補も戻らなければなりません。
//
// B を調べ始めた時点で A の候補は消えます。そこで A へ戻ったとき「A は調べ済み」
// として何もしないと、入力欄は A なのに候補は空、表示は「調べています…」のまま
// 取り残されます (B の応答は名前が違うので捨てられます)。
func TestSettingsPageBringsBackTheCandidatesWhenTheCameraComesBack(t *testing.T) {
	harness := modesHarness + `
// A を調べ終える。
const a = loadCameraModes();
release();
await a;
const afterA = listed["camera-sizes"];

// B を調べ始める。ここで A の候補は消える。
nodes["uvc-device"].value = "B";
const b = loadCameraModes();
const duringB = listed["camera-sizes"];

// A へ戻る。
nodes["uvc-device"].value = "A";
const back = loadCameraModes();
release();
await Promise.all([b, back]);
console.log(JSON.stringify({
  afterA, duringB,
  backSizes: listed["camera-sizes"],
  said: nodes["camera-modes"].textContent,
}));
`
	var got struct {
		AfterA    []string `json:"afterA"`
		DuringB   []string `json:"duringB"`
		BackSizes []string `json:"backSizes"`
		Said      string   `json:"said"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if want := []string{"640x480"}; !equalStrings(got.AfterA, want) {
		t.Fatalf("sizes after looking up A = %v, want %v", got.AfterA, want)
	}
	if len(got.DuringB) != 0 {
		t.Fatalf("sizes while looking up B = %v, want A's candidates gone", got.DuringB)
	}
	if want := []string{"640x480"}; !equalStrings(got.BackSizes, want) {
		t.Errorf("sizes after coming back to A = %v, want %v", got.BackSizes, want)
	}
	if got.Said == "looking" {
		t.Errorf("the page is still saying %q after coming back to A", got.Said)
	}
}

// 進行中は 1 つでは足りません。
//
// A を待っている間に B へ変え、また A に戻すと、3 回目の A は「今 B を調べている」
// という印をすり抜けます。同時に走り得るのは、走っている数だけあります。
func TestSettingsPageRemembersEveryLookupStillRunning(t *testing.T) {
	harness := modesHarness + `
const first = loadCameraModes();
nodes["uvc-device"].value = "B";
const second = loadCameraModes();
// 打ち直して A に戻る。A はまだ走っている。
nodes["uvc-device"].value = "A";
const third = loadCameraModes();
const started = asked;
release();
await Promise.all([first, second, third]);
console.log(JSON.stringify({started}));
`
	var got struct {
		Started int `json:"started"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if got.Started != 2 {
		t.Errorf("started %d lookups, want 2 — the one that came back to A must not start a second time", got.Started)
	}
}

// 古い要求が転んでも、今のカメラについて得たものを捨ててはいけません。
//
// 成功の側には古さの判定がありますが、例外の側にもそれが要ります。無いと、
// 入力欄は B のまま A の失敗が出て、B の候補は次に入力の合図が来るまで戻りません。
func TestSettingsPageIgnoresTheFailureOfALookupItNoLongerNeeds(t *testing.T) {
	harness := `
const TEXT = {modesUnknown: "unknown", modesFound: "found", modesLooking: "looking"};
globalThis.document = { querySelector: () => ({ value: "uvc" }) };
const nodes = {"uvc-device": {value: "A"}, "uvc-size": {value: ""}, "camera-modes": {textContent: ""}};
const el = (id) => nodes[id] || (nodes[id] = {value: "", textContent: ""});
const listed = {"camera-sizes": [], "camera-framerates": []};
const options = (id, values) => { listed[id] = values; };

// A は返らないまま後で転ぶ。B はすぐ答える。
let breakA;
globalThis.fetch = async (url) => {
  if (decodeURIComponent(url).includes("device=A")) {
    await new Promise((resolve, reject) => { breakA = reject; });
  }
  return { ok: true, json: async () => ({
    device: "B",
    modes: [{min_size: "1280x720", max_size: "1280x720", min_fps: 30, max_fps: 30}],
  }) };
};

const a = loadCameraModes();
nodes["uvc-device"].value = "B";
await loadCameraModes();
const afterB = listed["camera-sizes"];

breakA(new Error("A broke"));
await a;
console.log(JSON.stringify({afterB, sizes: listed["camera-sizes"], said: nodes["camera-modes"].textContent}));
`
	var got struct {
		AfterB []string `json:"afterB"`
		Sizes  []string `json:"sizes"`
		Said   string   `json:"said"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if want := []string{"1280x720"}; !equalStrings(got.AfterB, want) {
		t.Fatalf("sizes after B answered = %v, want %v", got.AfterB, want)
	}
	if want := []string{"1280x720"}; !equalStrings(got.Sizes, want) {
		t.Errorf("sizes after the stale lookup failed = %v, want B's %v", got.Sizes, want)
	}
	if strings.Contains(got.Said, "A broke") {
		t.Errorf("the page said %q, want no complaint about a camera it is no longer showing", got.Said)
	}
}

// UVC を使っていないなら、カメラを開いてはいけません。
//
// 設定には前に使ったカメラ名が残り、fill() はそれを隠れている入力欄にも書きます。
// そのまま調べに行くと、設定画面を開いただけで、使ってもいないカメラを他のアプリ
// と取り合うことになります。
func TestSettingsPageLeavesTheCameraAloneWhenAnotherSourceIsChosen(t *testing.T) {
	harness := modesHarness + `
sourceType = "serial";
const quiet = loadCameraModes();
const whileSerial = asked;
release();
await quiet;

// UVC に切り替えたら、そこで初めて調べる。
sourceType = "uvc";
const now = loadCameraModes();
release();
await now;
console.log(JSON.stringify({whileSerial, afterSwitch: asked}));
`
	var got struct {
		WhileSerial int `json:"whileSerial"`
		AfterSwitch int `json:"afterSwitch"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.WhileSerial != 0 {
		t.Errorf("opened the camera %d times while the source was serial, want 0", got.WhileSerial)
	}
	if got.AfterSwitch != 1 {
		t.Errorf("looked up %d times after switching to uvc, want 1", got.AfterSwitch)
	}
}

// フォームを書き直したら、候補も取り直さなければなりません。
//
// 代入では change も blur も起きません。読み込み時だけの話ではなく、保存のたびに
// 通る経路でもあります — トレイや別のクライアントがカメラを変えていれば、応答が
// この欄をそちらへ書き換えるので、欄と候補が別のカメラを指したままになります。
func TestSettingsPageLooksUpTheModesWheneverItRewritesTheForm(t *testing.T) {
	for _, name := range []string{"function fill(cfg) {", "function rebase(cfg, keep) {"} {
		body := settingsFunction(t, name)
		if !strings.Contains(body, "loadCameraModes()") {
			t.Errorf("%s does not look up the camera modes; the events the page listens for do not fire when the form fills itself", name)
		}
	}
}

// settingsFunction は、画面のスクリプトから 1 つの関数の中身を切り出します。
// 終わりは行頭の "}" — このファイルの関数はすべてその形で閉じています。
func settingsFunction(t *testing.T, header string) string {
	t.Helper()
	start := strings.Index(uiSettingsHTML, header)
	if start < 0 {
		t.Fatalf("could not find %q in the settings page", header)
	}
	rest := uiSettingsHTML[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("could not find the end of %q", header)
	}
	return rest[:end]
}

// 同じフレンドリ名のカメラが 2 台あるときは、見分けられる名前も候補に出さなければ
// なりません。どちらを選んでも同じ要求になり、モードの問い合わせもキャプチャも
// 常に同じ 1 台を開くためです。重複していないカメラには足しません — 長い名前は、
// それが要る人にだけ見せます。
func TestSettingsPageOffersTheDevicePathOnlyWhenNamesCollide(t *testing.T) {
	harness := `
console.log(JSON.stringify({
  unique: cameraChoices([
    {name: "Bigeye", alternative: "@device_pnp_one"},
    {name: "Webcam", alternative: "@device_pnp_two"},
  ]),
  collided: cameraChoices([
    {name: "USB Camera", alternative: "@device_pnp_one"},
    {name: "USB Camera", alternative: "@device_pnp_two"},
  ]),
  bare: cameraChoices([{name: "Bigeye"}]),
}));
`
	var got struct {
		Unique   []string `json:"unique"`
		Collided []string `json:"collided"`
		Bare     []string `json:"bare"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if want := []string{"Bigeye", "Webcam"}; !equalStrings(got.Unique, want) {
		t.Errorf("choices for cameras with their own names = %v, want %v", got.Unique, want)
	}
	if want := []string{"USB Camera", "@device_pnp_one", "USB Camera", "@device_pnp_two"}; !equalStrings(got.Collided, want) {
		t.Errorf("choices for two cameras sharing a name = %v, want %v", got.Collided, want)
	}
	if want := []string{"Bigeye"}; !equalStrings(got.Bare, want) {
		t.Errorf("choices for a camera with no device path = %v, want %v", got.Bare, want)
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
