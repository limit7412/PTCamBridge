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
const TEXT = {modesNoSize: "no size", modesNoRate: "no rate"};
let chosen = "";
const el = () => ({ value: chosen, setCustomValidity() {} });
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
const TEXT = {modesNoSize: "no size", modesNoRate: "no rate"};
let chosen = "";
const el = () => ({ value: chosen, setCustomValidity() {} });
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
const TEXT = {modesNoSize: "no size", modesNoRate: "no rate"};
let chosen = "640x480";
const el = () => ({ value: chosen, setCustomValidity() {} });
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
const field = (value) => ({ value, textContent: "", invalid: "", setCustomValidity(why) { this.invalid = why; } });
const nodes = {"uvc-device": field("Bigeye"), "uvc-size": field(""), "camera-modes": field("")};
const el = (id) => nodes[id] || (nodes[id] = field(""));
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
const TEXT = {modesUnknown: "unknown", modesFound: "found", modesLooking: "looking", modesNoSize: "no size", modesNoRate: "no rate"};
let sourceType = "uvc";
globalThis.document = { querySelector: () => ({ value: sourceType }) };
const field = (value) => ({ value, textContent: "", invalid: "", setCustomValidity(why) { this.invalid = why; } });
const nodes = {"uvc-device": field("A"), "uvc-size": field(""), "uvc-framerate": field(""), "camera-modes": field("")};
const el = (id) => nodes[id] || (nodes[id] = field(""));
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

// まだ答えていないカメラへ戻ったときも、出ている候補は捨てなければなりません。
//
// A を調べている最中に B へ変え、B が先に答えると、画面には B の候補が出ます。
// そこで A へ戻ると「A は調べ中」として何もしないままになり、入力欄は A なのに
// B の解像度が選べます。選んで保存すれば、A が持たないモードが設定に入ります。
func TestSettingsPageDropsAnotherCamerasCandidatesWhenComingBackMidLookup(t *testing.T) {
	harness := modesHarness + `
// A を調べ始める。まだ答えない。
const a = loadCameraModes();

// B へ変えて、B だけを答えさせる。
nodes["uvc-device"].value = "B";
const b = loadCameraModes();
const waitingForA = pending.slice(0, 1);
pending = pending.slice(1);
release();
await b;
const afterB = listed["camera-sizes"];

// A へ戻る。A はまだ調べている最中。
nodes["uvc-device"].value = "A";
await loadCameraModes();
const backOnA = { sizes: listed["camera-sizes"], said: nodes["camera-modes"].textContent, asked };

pending = waitingForA;
release();
await a;
console.log(JSON.stringify({afterB, backOnA, finalSizes: listed["camera-sizes"]}));
`
	var got struct {
		AfterB  []string `json:"afterB"`
		BackOnA struct {
			Sizes []string `json:"sizes"`
			Said  string   `json:"said"`
			Asked int      `json:"asked"`
		} `json:"backOnA"`
		FinalSizes []string `json:"finalSizes"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if want := []string{"1280x720"}; !equalStrings(got.AfterB, want) {
		t.Fatalf("sizes after B answered = %v, want %v", got.AfterB, want)
	}
	if len(got.BackOnA.Sizes) != 0 {
		t.Errorf("sizes after coming back to A = %v, want B's candidates gone", got.BackOnA.Sizes)
	}
	if got.BackOnA.Said != "looking" {
		t.Errorf("the page said %q while A is still being looked up, want it to say so", got.BackOnA.Said)
	}
	if got.BackOnA.Asked != 2 {
		t.Errorf("started %d lookups, want the one already running for A to be reused", got.BackOnA.Asked)
	}
	// A が答えたら、A の候補で埋まる。
	if want := []string{"640x480"}; !equalStrings(got.FinalSizes, want) {
		t.Errorf("sizes once A answered = %v, want %v", got.FinalSizes, want)
	}
}

// 同じカメラのままでも、解像度が変われば候補は作り直さなければなりません。
//
// 別のクライアントが解像度だけを変えると、保存の応答が `fill()` を通ってこの欄を
// 書き換えます。代入では `input` も起きないので、ここで拾わなければ、新しい解像度に
// 前の解像度のフレームレートが並んだままになります。
func TestSettingsPageRebuildsTheChoicesWhenOnlyTheSizeChanged(t *testing.T) {
	harness := modesHarness + `
// このカメラは既に調べ済み。2 つの解像度でフレームレートが違う。
modesFor = "A";
cameraModes = [
  {min_size: "640x480", max_size: "640x480", min_fps: 30, max_fps: 30},
  {min_size: "1920x1080", max_size: "1920x1080", min_fps: 5, max_fps: 5},
];
nodes["uvc-size"].value = "640x480";
await loadCameraModes();
const small = listed["camera-framerates"];

// fill() が解像度だけを書き換えた。カメラ名は同じ。
nodes["uvc-size"].value = "1920x1080";
await loadCameraModes();
console.log(JSON.stringify({small, big: listed["camera-framerates"], asked}));
`
	var got struct {
		Small []string `json:"small"`
		Big   []string `json:"big"`
		Asked int      `json:"asked"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if want := []string{"30"}; !equalStrings(got.Small, want) {
		t.Fatalf("framerates for 640x480 = %v, want %v", got.Small, want)
	}
	if want := []string{"5"}; !equalStrings(got.Big, want) {
		t.Errorf("framerates after the size changed = %v, want %v", got.Big, want)
	}
	if got.Asked != 0 {
		t.Errorf("asked the server %d times, want none — the modes it already has are enough", got.Asked)
	}
}

// 候補欄は入力を縛りません。カメラが持っていない組み合わせのまま保存できては
// いけません。
//
// 640x480@30 の状態から、30fps を持たない 1920x1080 へ解像度だけを打ち替えると、
// フレームレートの欄には 30 が残ります。そのまま保存すると、この PR が説明しよう
// としている失敗そのものを起こします。
func TestSettingsPageRefusesACombinationTheCameraDoesNotHave(t *testing.T) {
	harness := modesHarness + `
cameraModes = [
  {min_size: "640x480", max_size: "640x480", min_fps: 30, max_fps: 30},
  {min_size: "1920x1080", max_size: "1920x1080", min_fps: 5, max_fps: 5},
];
const state = () => ({ size: nodes["uvc-size"].invalid, fps: nodes["uvc-framerate"].invalid });

nodes["uvc-size"].value = "640x480";
nodes["uvc-framerate"].value = "30";
refreshModeChoices();
const ok = state();

// 解像度だけを打ち替えた。30fps はこの解像度には無い。
nodes["uvc-size"].value = "1920x1080";
refreshModeChoices();
const mismatch = state();

// カメラ任せに戻せば通る。
nodes["uvc-framerate"].value = "0";
refreshModeChoices();
const cleared = state();

// このカメラが持っていない解像度そのものも断る。
nodes["uvc-size"].value = "320x240";
refreshModeChoices();
const unknownSize = state();

// モードを知らないカメラについては、何も言わない。
cameraModes = [];
nodes["uvc-framerate"].value = "30";
refreshModeChoices();
console.log(JSON.stringify({ok, mismatch, cleared, unknownSize, unknownCamera: state()}));
`
	type validity struct {
		Size string `json:"size"`
		FPS  string `json:"fps"`
	}
	var got struct {
		OK            validity `json:"ok"`
		Mismatch      validity `json:"mismatch"`
		Cleared       validity `json:"cleared"`
		UnknownSize   validity `json:"unknownSize"`
		UnknownCamera validity `json:"unknownCamera"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.OK != (validity{}) {
		t.Errorf("a combination the camera has was refused: %+v", got.OK)
	}
	if got.Mismatch.FPS == "" {
		t.Error("a framerate the chosen size does not offer was accepted")
	}
	if got.Cleared.FPS != "" {
		t.Errorf("leaving the framerate to the camera was refused: %q", got.Cleared.FPS)
	}
	if got.UnknownSize.Size == "" {
		t.Error("a size the camera does not have was accepted")
	}
	if got.UnknownCamera != (validity{}) {
		t.Errorf("a camera whose modes are unknown was judged: %+v", got.UnknownCamera)
	}
}

// 判定はモードの範囲そのもので行わなければなりません。
//
// 候補は「書ける整数」に絞ってあるので、そちらと突き合わせると、5-30fps の
// カメラに入っている 15 のような正しい値まで弾きます。しかも保存できなくなるのは
// その欄だけではありません — 無関係な項目も一緒に止まります。逆に 29.97fps しか
// 持たないカメラでは候補が空になるので、候補で見ると 30 が素通りします。
func TestSettingsPageJudgesFrameratesByTheModeNotTheShortlist(t *testing.T) {
	harness := modesHarness + `
const check = (modes, size, fps) => {
  cameraModes = modes;
  nodes["uvc-size"].value = size;
  nodes["uvc-framerate"].value = fps;
  refreshModeChoices();
  return nodes["uvc-framerate"].invalid;
};

const ranged = [{min_size: "640x480", max_size: "640x480", min_fps: 5, max_fps: 30}];
const fractional = [{min_size: "640x480", max_size: "640x480", min_fps: 29.97, max_fps: 29.97}];
console.log(JSON.stringify({
  inside: check(ranged, "640x480", "15"),
  atTheEdge: check(ranged, "640x480", "30"),
  outside: check(ranged, "640x480", "60"),
  aboveAFractionalOnly: check(fractional, "640x480", "30"),
}));
`
	var got struct {
		Inside               string `json:"inside"`
		AtTheEdge            string `json:"atTheEdge"`
		Outside              string `json:"outside"`
		AboveAFractionalOnly string `json:"aboveAFractionalOnly"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.Inside != "" {
		t.Errorf("15fps on a 5-30fps camera was refused (%q); it is inside the range even though it is not on the shortlist", got.Inside)
	}
	if got.AtTheEdge != "" {
		t.Errorf("30fps on a 5-30fps camera was refused: %q", got.AtTheEdge)
	}
	if got.Outside == "" {
		t.Error("60fps on a 5-30fps camera was accepted")
	}
	if got.AboveAFractionalOnly == "" {
		t.Error("30fps on a camera that only offers 29.97fps was accepted; its shortlist is empty, which is not the same as anything goes")
	}
}

// 拒否は、直したその場で解けなければなりません。カスタムエラーが残っている
// フォームは submit そのものが起きないので、聞いていない欄に拒否を置くと、
// 保存の入口が閉じたままになります。
func TestSettingsPageLetsGoOfTheRefusalWhenTheValueIsFixed(t *testing.T) {
	harness := modesHarness + `
cameraModes = [{min_size: "640x480", max_size: "640x480", min_fps: 5, max_fps: 30}];
nodes["uvc-size"].value = "640x480";
nodes["uvc-framerate"].value = "60";
refreshModeChoices();
const refused = nodes["uvc-framerate"].invalid;

// フレームレートだけを直す。解像度には触らない。
nodes["uvc-framerate"].value = "15";
refreshModeChoices();
const fixed = nodes["uvc-framerate"].invalid;

// UVC を使わなくなったら、隠れた欄の拒否も解く。
nodes["uvc-framerate"].value = "60";
refreshModeChoices();
sourceType = "serial";
await loadCameraModes();
console.log(JSON.stringify({refused, fixed, afterSwitch: nodes["uvc-framerate"].invalid}));
`
	var got struct {
		Refused     string `json:"refused"`
		Fixed       string `json:"fixed"`
		AfterSwitch string `json:"afterSwitch"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.Refused == "" {
		t.Fatal("60fps on a 5-30fps camera was accepted")
	}
	if got.Fixed != "" {
		t.Errorf("the refusal survived the fix: %q", got.Fixed)
	}
	if got.AfterSwitch != "" {
		t.Errorf("a hidden UVC field is still blocking the form after switching to another source: %q", got.AfterSwitch)
	}
}

// ソースを切り替えたら、どちらへ切り替えても loadCameraModes を通さなければ
// なりません。UVC を選んだ瞬間はそのカメラを調べてよくなる瞬間であり、UVC を
// やめた瞬間は隠れた欄の拒否を解く瞬間です。どちらの判断も中にあります。
func TestSettingsPageRunsTheModeLogicOnEverySourceChange(t *testing.T) {
	body := settingsFunction(t, `for (const radio of document.querySelectorAll('input[name="source-type"]')) {`)
	if strings.Contains(body, `if (radio.value === "uvc") loadCameraModes()`) {
		t.Error("the source switch must call loadCameraModes for every source; the branch that clears the refusals lives inside it")
	}
	if !strings.Contains(body, "loadCameraModes();") {
		t.Error("the source switch does not run the mode logic at all")
	}
}

// フレームレートの欄も、打っている最中に聞いていなければなりません。聞かなければ、
// 直しても拒否が残り、解像度を触るまで保存できません。
func TestSettingsPageListensToTheFramerateField(t *testing.T) {
	if !strings.Contains(uiSettingsHTML, `el("uvc-framerate").addEventListener("input", refreshModeChoices)`) {
		t.Error("the settings page must re-check the framerate as it is typed; a refusal it never revisits blocks the whole form")
	}
}

// 初回のモード取得は、デバイス一覧を読んだ後でなければなりません。
//
// ブリッジが「カメラが入れ替わった」ことを知るのは、一覧を数えたときです。先に
// モードを訊くと、入れ替わったカメラの古い答えを受け取り、成功として覚えます —
// ページを読み直しても直りません。
//
// ただし待たせるのはモードの問い合わせだけです。デバイスの列挙は ffmpeg を
// 起動するので遅ければ 15 秒かかり、設定の表示までそれを待たせると、カメラと
// 無関係な項目を直したい人まで足止めされます。
func TestSettingsPageWaitsForTheDeviceListBeforeAskingForModes(t *testing.T) {
	harness := modesHarness + `
let finishList;
devicesListed = new Promise((resolve) => { finishList = resolve; });

const first = loadCameraModes();
await new Promise((r) => setTimeout(r, 0));
const beforeList = asked;

finishList();
await new Promise((r) => setTimeout(r, 0));
const afterList = asked;

release();
await first;
console.log(JSON.stringify({beforeList, afterList}));
`
	var got struct {
		BeforeList int `json:"beforeList"`
		AfterList  int `json:"afterList"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.BeforeList != 0 {
		t.Errorf("asked for the modes %d times before the device list came back, want 0", got.BeforeList)
	}
	if got.AfterList != 1 {
		t.Errorf("asked for the modes %d times after the device list came back, want 1", got.AfterList)
	}

	// フォームの読み込みは一覧を待ちません。
	if !strings.Contains(uiSettingsHTML, "devicesListed = loadDevices();\nload();") {
		t.Error("the settings page must load the form alongside the device list, not after it")
	}
}

// 一覧を待っている間に画面が動いたら、もう要らないカメラは開きません。
//
// 待ちに入る前に掴んだ名前のまま問い合わせると、既に別のカメラへ移った人・
// UVC をやめた人のために、使っていないカメラの使用ランプを点け、他のアプリと
// 15 秒取り合うことになります。
func TestSettingsPageChecksAgainAfterWaitingForTheDeviceList(t *testing.T) {
	harness := modesHarness + `
let finishList;
const held = () => { devicesListed = new Promise((resolve) => { finishList = resolve; }); };

// カメラ名が変わった場合。
held();
const forA = loadCameraModes();
nodes["uvc-device"].value = "B";
finishList();
await new Promise((r) => setTimeout(r, 0));
const afterRename = asked;

// ソースを変えた場合。
nodes["uvc-device"].value = "C";
held();
const forC = loadCameraModes();
sourceType = "serial";
finishList();
await new Promise((r) => setTimeout(r, 0));
const afterSwitch = asked;

release();
await Promise.all([forA, forC]);
console.log(JSON.stringify({afterRename, afterSwitch}));
`
	var got struct {
		AfterRename int `json:"afterRename"`
		AfterSwitch int `json:"afterSwitch"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.AfterRename != 0 {
		t.Errorf("opened %d cameras after the name moved on, want 0", got.AfterRename)
	}
	if got.AfterSwitch != 0 {
		t.Errorf("opened %d cameras after the source moved on, want 0", got.AfterSwitch)
	}
}

// 進行中は 1 つでは足りません。// 進行中は 1 つでは足りません。
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
const field = (value) => ({ value, textContent: "", invalid: "", setCustomValidity(why) { this.invalid = why; } });
const nodes = {"uvc-device": field("A"), "uvc-size": field(""), "uvc-framerate": field(""), "camera-modes": field("")};
const el = (id) => nodes[id] || (nodes[id] = field(""));
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

// UVC をやめた後に届いた応答を、採り込んではいけません。
//
// 離れた時点で隠れた欄の拒否は解いてあります。そこへ古い応答が入ると、拒否が
// 戻ってきます。見えない欄なので直しようがなく、別のソースの設定が保存できなく
// なります。名前の照合だけでは足りません — 名前は離れても変わらないからです。
func TestSettingsPageDropsTheAnswerThatArrivesAfterLeavingUVC(t *testing.T) {
	harness := modesHarness + `
// 応答に含まれない解像度。採り込めば必ず拒否になる。
nodes["uvc-size"].value = "1920x1080";
const inFlight = loadCameraModes();

// 返ってくる前に別のソースへ移る。移った側の経路が拒否を解く。
sourceType = "serial";
await loadCameraModes();
const afterSwitch = nodes["uvc-size"].invalid;

release();
await inFlight;
console.log(JSON.stringify({afterSwitch, afterStaleAnswer: nodes["uvc-size"].invalid, kept: cameraModes.length}));
`
	var got struct {
		AfterSwitch      string `json:"afterSwitch"`
		AfterStaleAnswer string `json:"afterStaleAnswer"`
		Kept             int    `json:"kept"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.AfterSwitch != "" {
		t.Fatalf("leaving uvc did not clear the refusal: %q", got.AfterSwitch)
	}
	if got.AfterStaleAnswer != "" {
		t.Errorf("a hidden uvc field is blocking the form again after a late answer: %q", got.AfterStaleAnswer)
	}
	if got.Kept != 0 {
		t.Errorf("kept %d modes of a camera the page no longer uses, want 0", got.Kept)
	}
}

// 例外の側にも同じ判定が要ります。失敗を採り込むと、UVC を使っていない画面に
// 「調べられませんでした」が出たままになります。
func TestSettingsPageDropsTheFailureThatArrivesAfterLeavingUVC(t *testing.T) {
	harness := `
const TEXT = {modesUnknown: "unknown", modesFound: "found", modesLooking: "looking", modesNoSize: "no size", modesNoRate: "no rate"};
let sourceType = "uvc";
globalThis.document = { querySelector: () => ({ value: sourceType }) };
const field = (value) => ({ value, textContent: "", invalid: "", setCustomValidity(why) { this.invalid = why; } });
const nodes = {"uvc-device": field("A"), "uvc-size": field(""), "uvc-framerate": field(""), "camera-modes": field("")};
const el = (id) => nodes[id] || (nodes[id] = field(""));
const listed = {"camera-sizes": [], "camera-framerates": []};
const options = (id, values) => { listed[id] = values; };

let pending = [];
const release = () => { const waiting = pending; pending = []; for (const reject of waiting) reject(new Error("boom")); };
globalThis.fetch = async () => {
  await new Promise((resolve, reject) => { pending.push(reject); });
};

const inFlight = loadCameraModes();
sourceType = "serial";
await loadCameraModes();
nodes["camera-modes"].textContent = "";
release();
await inFlight;
console.log(JSON.stringify({said: nodes["camera-modes"].textContent}));
`
	var got struct {
		Said string `json:"said"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if got.Said != "" {
		t.Errorf("reported %q about a camera the page no longer uses", got.Said)
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
  cased: cameraChoices([
    {name: "USB Camera", alternative: "@device_pnp_one"},
    {name: "usb camera", alternative: "@device_pnp_two"},
  ]),
}));
`
	var got struct {
		Unique   []string `json:"unique"`
		Collided []string `json:"collided"`
		Bare     []string `json:"bare"`
		Cased    []string `json:"cased"`
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
	// DirectShow のフレンドリ名は大文字小文字を区別しません。区別して数えると
	// どちらも 1 台と見なされ、候補には同じ 1 台に解決される名前しか出ません。
	if want := []string{"USB Camera", "@device_pnp_one", "usb camera", "@device_pnp_two"}; !equalStrings(got.Cased, want) {
		t.Errorf("choices for two cameras whose names differ only in case = %v, want %v", got.Cased, want)
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
