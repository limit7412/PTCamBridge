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

// 数えられた一覧として解く。数えられなかった一覧は、待っていた側も
// 引き下がらなければならない (別のテストで見ている)。
finishList(true);
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

	// フォームの読み込みは一覧を待ちません。数え直しは同期のうちに devicesListed を
	// 置いてから待ちに入るので、load() は先に進めます。
	if !strings.Contains(uiSettingsHTML, "countCameras();\nload();") {
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
	for _, name := range []string{"function fill(cfg, revision) {", "function rebase(cfg, keep, revision) {"} {
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

// 名前は合図になりません。同じフレンドリ名の別機種に差し替えられても変わらない
// からです。代わりに見るのはキャプチャの様子で、合図は 3 つあります。
//
// **抜いてから挿すまでの間に数え直しても、そこにはまだ新しいカメラがいません。**
// 切れたことだけを合図にすると、その一覧で終わってしまい、後から挿さった別機種は
// 誰にも気づかれません。繋がったことも合図に要ります。
func TestSettingsPageCountsTheCamerasOnEverySignThatTheyChanged(t *testing.T) {
	harness := modesHarness + `
let listings = 0;
const askedModes = globalThis.fetch;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    listings++;
    return { ok: true, json: async () => ({cameras: [], serial_ports: []}) };
  }
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };
const running = (over) => Object.assign({capturing: "uvc", reconnects: 1, connected: true, paused: false}, over);

// 最初の 1 回は合図にならない。比べる相手が無い。
const first = noticeCameras(running());
await settle();
const afterFirst = listings;

// 何も動かなければ数え直さない。
const same = noticeCameras(running());
await settle();

// 切れた。
const dropped = noticeCameras(running({reconnects: 2, connected: false}));
await settle();
const afterDrop = listings;

// 挿さって繋がった。抜いている間の一覧には、新しいカメラは載っていない。
const back = noticeCameras(running({reconnects: 2, connected: true}));
await settle();
const afterBack = listings;

// 一時停止して差し替え、再開した。止めている間の差し替えでは、ドライバは命じられて
// 止まっているので切れたことにならない。まだフレームも来ていない (connected は
// false のまま) ので、繋がったことも合図にならない。止まっている間は数えない。
noticeCameras(running({reconnects: 2, connected: false, paused: true, pauses: 1}));
await settle();
const beforeResume = listings;
const resumed = noticeCameras(running({reconnects: 2, connected: false, paused: false, pauses: 1}));
await settle();

// 止まったまま数え直しても、動き出したことは合図として残ります。
//
// 止まっている間にソースを切り替えると、それも合図なので数え直します。そこで
// 止められた分まで使い切ると、**その後に差し替えて再開しても**、数はもう動いた後
// なので合図が出ません。差し替えは止めている間のどこで起きてもおかしくありません。
const beforePausedSwitch = listings;
noticeCameras(running({reconnects: 2, connected: false, paused: true, pauses: 3, switches: 0}));
await settle();
const pausedSwitch = noticeCameras(running({reconnects: 2, connected: false, paused: true, pauses: 3, switches: 1}));
await settle();
const afterPausedSwitch = listings - beforePausedSwitch;

// 動き出した。止めている間に差し替えられていても、新しいカメラが今のモードを
// 受け付けなければ繋がらないので、他の合図は出ない。
const beforeWoke = listings;
const woke = noticeCameras(running({reconnects: 2, connected: false, paused: false, pauses: 3, switches: 1}));
await settle();
const afterWoke = listings - beforeWoke;

// 止めて差し替えて再開するまでが、読みと読みの間で終わった。前後の標本はどちらも
// 動いていて、命令による停止では reconnects も増えないので、今の状態を比べるだけ
// では何も変わって見えない。背景のタブでは読む間隔が分単位まで伸びるので、これは
// 十分あり得る。
const beforeQuick = listings;
const quick = noticeCameras(running({reconnects: 2, connected: false, paused: false, pauses: 4, switches: 1}));
await settle();
const afterQuick = listings - beforeQuick;

// 別のソースへ移り、そこで同名のカメラを差し替えて UVC へ戻した。ソースの切替では
// reconnects は増えず (status.SetSource は数を触らない)、次に見るまでに繋がって
// いれば connected も動かないので、他の合図はどれも出ない。
noticeCameras({capturing: "serial", reconnects: 2, connected: true, paused: false, pauses: 4, switches: 2});
await settle();
const beforeReturn = listings;
const returned = noticeCameras({capturing: "uvc", reconnects: 2, connected: true, paused: false, pauses: 4, switches: 3});
await settle();

// 移って戻るまでが、読みと読みの間で終わった。前後の種別はどちらも uvc で、
// 切替では切れた回数も増えない。数だけが動く。
const beforeRound = listings;
const roundTrip = noticeCameras({capturing: "uvc", reconnects: 2, connected: true, paused: false, pauses: 4, switches: 5});
await settle();
const afterRound = listings - beforeRound;

// 一時停止するとソースは空になる (bridge.stopLocked)。その標本は「UVC ではない」
// ので見送るが、そこで止められた分を使い切ってはいけない。立て直しでは移った回数も
// 動かないので、代わりになる合図が無い。
const beforeStopped = listings;
noticeCameras({capturing: "", reconnects: 2, connected: false, paused: true, pauses: 5, switches: 5});
await settle();
const whileStopped = listings - beforeStopped;

// 動き出した。止めている間に差し替えられていて、新しいカメラが今のモードを
// 受け付けなければ繋がらない。
const wokeFromStop = noticeCameras(running({reconnects: 2, connected: false, paused: false, pauses: 5, switches: 5}));
await settle();
const afterWokeFromStop = listings - beforeStopped - whileStopped;

console.log(JSON.stringify({first, afterFirst, same, dropped, afterDrop, back, afterBack, resumed, afterResume: beforePausedSwitch - beforeResume, pausedSwitch, afterPausedSwitch, woke, afterWoke, quick, afterQuick, returned, afterReturn: beforeRound - beforeReturn, roundTrip, afterRound, whileStopped, wokeFromStop, afterWokeFromStop}));
release();
`
	var got struct {
		First             bool `json:"first"`
		AfterFirst        int  `json:"afterFirst"`
		Same              bool `json:"same"`
		Dropped           bool `json:"dropped"`
		AfterDrop         int  `json:"afterDrop"`
		Back              bool `json:"back"`
		AfterBack         int  `json:"afterBack"`
		Resumed           bool `json:"resumed"`
		AfterResume       int  `json:"afterResume"`
		PausedSwitch      bool `json:"pausedSwitch"`
		AfterPausedSwitch int  `json:"afterPausedSwitch"`
		Woke              bool `json:"woke"`
		AfterWoke         int  `json:"afterWoke"`
		Quick             bool `json:"quick"`
		AfterQuick        int  `json:"afterQuick"`
		Returned          bool `json:"returned"`
		AfterReturn       int  `json:"afterReturn"`
		RoundTrip         bool `json:"roundTrip"`
		AfterRound        int  `json:"afterRound"`
		WhileStopped      int  `json:"whileStopped"`
		WokeFromStop      bool `json:"wokeFromStop"`
		AfterWokeFromStop int  `json:"afterWokeFromStop"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.First || got.AfterFirst != 0 {
		t.Errorf("the first reading counted the cameras (%d listings); there was nothing to compare it with", got.AfterFirst)
	}
	if got.Same {
		t.Error("counted the cameras again although nothing about the capture had moved")
	}
	if !got.Dropped || got.AfterDrop != 1 {
		t.Errorf("counted %d times after the camera dropped, want 1", got.AfterDrop)
	}
	if !got.Back || got.AfterBack != 2 {
		t.Errorf("counted %d times in total after it came back, want 2 — the listing taken while it was unplugged cannot hold the new camera", got.AfterBack)
	}
	if !got.Resumed || got.AfterResume != 1 {
		t.Errorf("counted %d times after resuming, want 1 — a swap while paused never disconnects anything", got.AfterResume)
	}
	if !got.PausedSwitch || got.AfterPausedSwitch != 1 {
		t.Errorf("counted %d times after switching sources while paused, want 1", got.AfterPausedSwitch)
	}
	if !got.Woke || got.AfterWoke != 1 {
		t.Errorf("counted %d times after it woke up, want 1 — counting while it was still paused must not spend the pause, because the swap can happen at any point before it wakes", got.AfterWoke)
	}
	if !got.Quick || got.AfterQuick != 1 {
		t.Errorf("counted %d times after a pause that started and ended between two readings, want 1 — comparing only the current state sees nothing", got.AfterQuick)
	}
	if !got.Returned || got.AfterReturn != 1 {
		t.Errorf("counted %d times after coming back to UVC, want 1 — switching sources moves neither the counter nor the connection", got.AfterReturn)
	}
	if !got.RoundTrip || got.AfterRound != 1 {
		t.Errorf("counted %d times after a trip through another source that started and ended between two readings, want 1 — both readings say uvc", got.AfterRound)
	}
	if got.WhileStopped != 0 {
		t.Errorf("counted %d times on a reading taken while the source was stopped, want 0", got.WhileStopped)
	}
	if !got.WokeFromStop || got.AfterWokeFromStop != 1 {
		t.Errorf("counted %d times after it woke from a stop, want 1 — pausing empties the source, and passing that reading over must not spend the pause", got.AfterWokeFromStop)
	}
}

// 数え直しは重ねません。列挙は 1 回に最大 15 秒かかるので、重ねると ffmpeg が
// 積み上がります。かといって捨てると、その間に起きた差し替えを永久に見落とします。
func TestSettingsPageCarriesOverASignalItCouldNotActOnYet(t *testing.T) {
	harness := modesHarness + `
let listings = 0;
let finish = () => {};
const askedModes = globalThis.fetch;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    listings++;
    await new Promise((resolve) => { finish = resolve; });
    return { ok: true, json: async () => ({cameras: [], serial_ports: []}) };
  }
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };
const running = (over) => Object.assign({capturing: "uvc", reconnects: 1, connected: true, paused: false}, over);

noticeCameras(running());
noticeCameras(running({reconnects: 2}));
await settle();
const whileRunning = listings;

// 1 本目がまだ終わっていない間に、もう一度切れる。
noticeCameras(running({reconnects: 3}));
await settle();
const stillOne = listings;

// 終われば、繰り越した分を 1 回だけ数え直す。
finish();
await settle();
finish();
await settle();
const carried = listings;

// 動いているのがカメラでなければ数え直さない。
noticeCameras({capturing: "serial", reconnects: 9, connected: true, paused: false});
await settle();
console.log(JSON.stringify({whileRunning, stillOne, carried, afterSerial: listings}));
release();
`
	var got struct {
		WhileRunning int `json:"whileRunning"`
		StillOne     int `json:"stillOne"`
		Carried      int `json:"carried"`
		AfterSerial  int `json:"afterSerial"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.WhileRunning != 1 || got.StillOne != 1 {
		t.Errorf("started %d listings while one was still running, want 1", got.StillOne)
	}
	if got.Carried != 2 {
		t.Errorf("counted %d times in total, want 2 — the signal that arrived mid-listing must be carried over, not dropped", got.Carried)
	}
	if got.AfterSerial != got.Carried {
		t.Error("counted the cameras again for a source that is not a camera")
	}
}

// 失敗した一覧は「1 台も無い」とは違います。ブリッジもそのときは素性を照らし
// 合わせないので、候補を捨てて訊き直しても、返るのは同じ古い憶えです。それを
// 成功として憶え直すと、次の合図が来るまで古い候補が残ります。
func TestSettingsPageKeepsAskingWhenTheCamerasCouldNotBeCounted(t *testing.T) {
	harness := modesHarness + `
let failing = true;
const askedModes = globalThis.fetch;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    return { ok: true, json: async () => (failing
      ? {cameras: [], serial_ports: [], camera_error: "uvc: ffmpeg is not installed"}
      : {cameras: [], serial_ports: []}) };
  }
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };
const running = (over) => Object.assign({capturing: "uvc", reconnects: 1, connected: true, paused: false}, over);

// A のモードは既に出ているものとする。
nodes["uvc-device"].value = "A";
modesFor = "A";
noticeCameras(running());
noticeCameras(running({reconnects: 2}));
await settle();
const afterFailure = modesFor;

// 列挙が直れば、次の合図で捨てて訊き直す。
failing = false;
noticeCameras(running({reconnects: 3}));
await settle();
console.log(JSON.stringify({afterFailure, afterSuccess: modesFor}));
release();
`
	var got struct {
		AfterFailure string `json:"afterFailure"`
		AfterSuccess string `json:"afterSuccess"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.AfterFailure != "A" {
		t.Errorf("modesFor = %q after a listing that failed, want the page not to record a fresh answer it never got", got.AfterFailure)
	}
	if got.AfterSuccess != "" {
		t.Errorf("modesFor = %q after the cameras could be counted, want the candidates dropped", got.AfterSuccess)
	}
}

// 繋がらないまま漂っている間は、合図が 1 つも出ないままカメラが差し替わります。
//
// 切れた時点で繋がっていなければ reconnects は増えず、新しいカメラが今の解像度を
// 受け付けなければ connected にもなりません。古い候補を見ながら映らないカメラを
// 差し替えるのがまさにこの状態なので、ここだけは間隔を空けて数え直します。
func TestSettingsPageKeepsCountingWhileTheCameraNeverComesBack(t *testing.T) {
	harness := modesHarness + `
let listings = 0;
const askedModes = globalThis.fetch;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    listings++;
    return { ok: true, json: async () => ({cameras: [], serial_ports: []}) };
  }
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };
const adrift = (over) => Object.assign({capturing: "uvc", reconnects: 1, connected: false, paused: false}, over);

// 時計はこちらが進める。実時間を待つテストは、待つ間に何も確かめない。
let clock = 1000000;
Date.now = () => clock;

// 漂っている状態を 2 回見せる。1 回目は比べる相手が無いので数えない。
noticeCameras(adrift());
await settle();
const afterFirst = listings;
noticeCameras(adrift());
await settle();
const afterSecond = listings;

// 間隔の内側では数え直さない。列挙は 1 回に最大 15 秒かかるので、2 秒ごとの
// polling でそのたびに数えると ffmpeg が常駐する。
clock += 29000;
noticeCameras(adrift());
await settle();
const tooSoon = listings;

// 間隔を越えたら数え直す。
clock += 2000;
noticeCameras(adrift());
await settle();
const later = listings;

// 一時停止中は数えない。止めたのはユーザーで、再開そのものが合図になる。
clock += 60000;
noticeCameras(adrift({paused: true}));
await settle();
const whilePaused = listings;

// 繋がったところは合図そのもの (挿さった)。ここでは数える。
clock += 60000;
noticeCameras(adrift({connected: true}));
await settle();
const onConnect = listings;

// 繋がったままなら、間隔をいくら空けても数えない。差し替えれば必ず切れるので、
// 3 つの合図がその場面を拾う。
clock += 60000;
noticeCameras(adrift({connected: true}));
await settle();
const whileConnected = listings;

console.log(JSON.stringify({afterFirst, afterSecond, tooSoon, later, whilePaused, onConnect, whileConnected}));
release();
`
	var got struct {
		AfterFirst     int `json:"afterFirst"`
		AfterSecond    int `json:"afterSecond"`
		TooSoon        int `json:"tooSoon"`
		Later          int `json:"later"`
		WhilePaused    int `json:"whilePaused"`
		OnConnect      int `json:"onConnect"`
		WhileConnected int `json:"whileConnected"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.AfterFirst != 0 {
		t.Errorf("the first reading counted the cameras (%d listings); there was nothing to compare it with", got.AfterFirst)
	}
	if got.AfterSecond != 1 {
		t.Errorf("counted %d times while the capture was adrift, want 1 — nothing else will ever signal a swap in this state", got.AfterSecond)
	}
	if got.TooSoon != 1 {
		t.Errorf("counted %d times inside the interval, want 1 — a listing takes up to 15s, so ffmpeg would never leave", got.TooSoon)
	}
	if got.Later != 2 {
		t.Errorf("counted %d times after the interval passed, want 2", got.Later)
	}
	if got.WhilePaused != 2 {
		t.Errorf("counted %d times while paused, want 2 — the user stopped it, and resuming is the signal", got.WhilePaused)
	}
	if got.OnConnect != 3 {
		t.Errorf("counted %d times when it finally connected, want 3 — that is the plugged-in signal", got.OnConnect)
	}
	if got.WhileConnected != 3 {
		t.Errorf("counted %d times while it stayed connected, want 3 — a swap always disconnects, so the three signals cover it", got.WhileConnected)
	}
}

// 数えられなかった一覧を待っていたモード問い合わせも、引き下がらなければなりません。
//
// 一覧が失敗したときブリッジは素性を照らし合わせないので、そこで訊くと差し替え前の
// 憶えを受け取り、それを調べ済みとして刻み直します。数え直しの側で止めても、初回の
// 表示から並行して走っている問い合わせはこの待ちを通ります。
func TestSettingsPageDoesNotAskForModesBehindAFailedListing(t *testing.T) {
	harness := modesHarness + `
const askedModes = globalThis.fetch;
let askedForModes = 0;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    return { ok: true, json: async () => ({cameras: [], serial_ports: [], camera_error: "uvc: ffmpeg is not installed"}) };
  }
  askedForModes++;
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };

// 初回の表示。数え直しと、それを待つモード問い合わせが同時に走る。
nodes["uvc-device"].value = "A";
countCameras();
loadCameraModes();
await settle();

console.log(JSON.stringify({askedForModes, modesFor: modesFor || ""}));
release();
`
	var got struct {
		AskedForModes int    `json:"askedForModes"`
		ModesFor      string `json:"modesFor"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.AskedForModes != 0 {
		t.Errorf("asked for modes %d times behind a listing that failed, want 0 — the answer can only be the memory from before the swap", got.AskedForModes)
	}
	if got.ModesFor != "" {
		t.Errorf("modesFor = %q, want none recorded — nothing was actually looked up", got.ModesFor)
	}
}

// 訊き直すのは、繰り越しまで終えた後です。
//
// 途中の一覧で始めると、繰り越した列挙と同じカメラを同時に開きに行くことになります。
// しかも途中の一覧が差し替え前のもので、繰り越した側だけが転んだ場合、世代が進まない
// ので、その間に返った差し替え前の答えが有効なものとして受理されます。
func TestSettingsPageWaitsForTheLastListingBeforeAskingAgain(t *testing.T) {
	harness := modesHarness + `
let listings = 0;
let askedForModes = 0;
let devicePending = [];
const releaseDevices = () => { const waiting = devicePending; devicePending = []; for (const resolve of waiting) resolve(); };
const askedModes = globalThis.fetch;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    listings++;
    await new Promise((resolve) => { devicePending.push(resolve); });
    return { ok: true, json: async () => ({cameras: [], serial_ports: []}) };
  }
  askedForModes++;
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };
const running = (over) => Object.assign({capturing: "uvc", reconnects: 1, connected: true, paused: false}, over);

nodes["uvc-device"].value = "A";
noticeCameras(running());
noticeCameras(running({reconnects: 2}));
await settle();
const started = {listings, askedForModes};

// 走っている間にもう一度合図。繰り越される。
noticeCameras(running({reconnects: 3}));
await settle();

// 1 本目が終わる。ここで訊きに行ってはいけない — 2 本目がこれから走る。
releaseDevices();
await settle();
const between = {listings, askedForModes};

// 2 本目が終わる。ここで初めて訊く。
releaseDevices();
await settle();
const after = {listings, askedForModes};

console.log(JSON.stringify({started, between, after}));
release();
`
	type step struct {
		Listings      int `json:"listings"`
		AskedForModes int `json:"askedForModes"`
	}
	var got struct {
		Started step `json:"started"`
		Between step `json:"between"`
		After   step `json:"after"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.Started.Listings != 1 || got.Started.AskedForModes != 0 {
		t.Fatalf("after the signal: %d listings and %d mode lookups, want 1 and 0", got.Started.Listings, got.Started.AskedForModes)
	}
	if got.Between.Listings != 2 {
		t.Errorf("the carried-over listing did not start (%d listings, want 2)", got.Between.Listings)
	}
	if got.Between.AskedForModes != 0 {
		t.Errorf("asked for the modes %d times while the carried-over listing was still running, want 0 — both would open the same camera, and a listing that fails afterwards leaves that answer standing", got.Between.AskedForModes)
	}
	if got.After.AskedForModes != 1 {
		t.Errorf("asked for the modes %d times after the last listing, want 1", got.After.AskedForModes)
	}
}

// 列挙を待っているモードの問い合わせも、繰り越した最後の一覧まで待ちます。
//
// 待ち始めた時点の約束を掴んだままだと、繰り越しで入れ替わったことに気づけません。
// 1 本目が解けた時点で訊きに行き、2 本目の列挙と同じカメラを開く ffmpeg が並びます。
func TestSettingsPageWaitsForTheCarriedOverListingToo(t *testing.T) {
	harness := modesHarness + `
let listings = 0;
let askedForModes = 0;
let devicePending = [];
const releaseDevices = () => { const waiting = devicePending; devicePending = []; for (const resolve of waiting) resolve(); };
const askedModes = globalThis.fetch;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    listings++;
    await new Promise((resolve) => { devicePending.push(resolve); });
    return { ok: true, json: async () => ({cameras: [], serial_ports: []}) };
  }
  askedForModes++;
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };
const running = (over) => Object.assign({capturing: "uvc", reconnects: 1, connected: true, paused: false, pauses: 0}, over);

nodes["uvc-device"].value = "A";
noticeCameras(running());
noticeCameras(running({reconnects: 2}));
await settle();

// 列挙を待つモードの問い合わせ。カメラ名の change/blur から起きる。
loadCameraModes();
await settle();
const waiting = askedForModes;

// 走っている間にもう一度合図。繰り越される。
noticeCameras(running({reconnects: 3}));
await settle();

// 1 本目が終わる。掴んだ約束はここで解けるが、まだ訊きに行ってはいけない。
releaseDevices();
await settle();
const between = {listings, askedForModes};

// 2 本目が終わる。ここで初めて訊く。
releaseDevices();
await settle();
const after = askedForModes;

console.log(JSON.stringify({waiting, between, after}));
release();
`
	var got struct {
		Waiting int `json:"waiting"`
		Between struct {
			Listings      int `json:"listings"`
			AskedForModes int `json:"askedForModes"`
		} `json:"between"`
		After int `json:"after"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.Waiting != 0 {
		t.Fatalf("asked for the modes %d times while the first listing was still running, want 0", got.Waiting)
	}
	if got.Between.Listings != 2 {
		t.Errorf("the carried-over listing did not start (%d listings, want 2)", got.Between.Listings)
	}
	if got.Between.AskedForModes != 0 {
		t.Errorf("asked for the modes %d times once the first listing resolved, want 0 — the carried-over listing is still running and would open the same camera", got.Between.AskedForModes)
	}
	if got.After != 1 {
		t.Errorf("asked for the modes %d times after the last listing, want 1", got.After)
	}
}

// 途中で一度でも数えられていれば、憶えはもう古いので捨てます。
//
// ブリッジは列挙できたときに素性を照らし合わせます。繰り越しの途中で一度成功して
// いれば、その時点で差し替えは知られています。最後の一覧が転んだからといって候補を
// 残すと、差し替え前のものが次の数え直しまで有効なまま居座ります。
func TestSettingsPageDropsTheCandidatesEvenIfTheLastListingFailed(t *testing.T) {
	harness := modesHarness + `
let listings = 0;
let askedForModes = 0;
let devicePending = [];
const releaseDevices = () => { const waiting = devicePending; devicePending = []; for (const resolve of waiting) resolve(); };
const askedModes = globalThis.fetch;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    const which = ++listings;
    await new Promise((resolve) => { devicePending.push(resolve); });
    // 1 本目は数えられる。繰り越した 2 本目だけが転ぶ。
    return { ok: true, json: async () => (which === 1
      ? {cameras: [], serial_ports: []}
      : {cameras: [], serial_ports: [], camera_error: "uvc: ffmpeg is not installed"}) };
  }
  askedForModes++;
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };
const running = (over) => Object.assign({capturing: "uvc", reconnects: 1, connected: true, paused: false, pauses: 0, switches: 0}, over);

// A は調べ終えていて、候補も出ているものとする。
nodes["uvc-device"].value = "A";
modesFor = "A";
cameraModes = [{min_size: "640x480", max_size: "640x480", min_fps: 30, max_fps: 30}];
refreshModeChoices();

noticeCameras(running());
noticeCameras(running({reconnects: 2}));
await settle();

// 走っている間にもう一度合図。繰り越される。
noticeCameras(running({reconnects: 3}));
await settle();

releaseDevices();   // 1 本目 (数えられる)
await settle();
releaseDevices();   // 2 本目 (転ぶ)
await settle();

console.log(JSON.stringify({listings, askedForModes, modesFor: modesFor || "", sizes: listed["camera-sizes"] || []}));
release();
`
	var got struct {
		Listings      int      `json:"listings"`
		AskedForModes int      `json:"askedForModes"`
		ModesFor      string   `json:"modesFor"`
		Sizes         []string `json:"sizes"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.Listings != 2 {
		t.Fatalf("%d listings, want 2 — the signal that arrived mid-run must be carried over", got.Listings)
	}
	if got.ModesFor != "" {
		t.Errorf("modesFor = %q, want it dropped — a listing in this run did succeed, so the bridge already reconciled and this memory is from before the swap", got.ModesFor)
	}
	if got.AskedForModes != 0 {
		t.Errorf("asked for the modes %d times, want 0 — the last listing failed, so the answer would not have been reconciled either", got.AskedForModes)
	}
	if len(got.Sizes) != 0 {
		t.Errorf("the size choices are still %v, want none — dropping only the mark leaves the sizes from before the swap selectable", got.Sizes)
	}
}

// 世代は、数えられた**その瞬間**に進めます。
//
// 繰り越しの 1 本目が数えられた時点で、ブリッジは素性を照らし合わせています。それより
// 前に立った問い合わせの答えは、もう差し替え前のものです。ループを抜けるまで待つと、
// 繰り越しが走っている間に届いたその答えが、今の世代のものとして受理されます。
func TestSettingsPageDropsAnAnswerThatLandsMidRecount(t *testing.T) {
	harness := modesHarness + `
let listings = 0;
let devicePending = [];
const releaseDevices = () => { const waiting = devicePending; devicePending = []; for (const resolve of waiting) resolve(); };
const askedModes = globalThis.fetch;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    const which = ++listings;
    await new Promise((resolve) => { devicePending.push(resolve); });
    // 1 本目は数えられる。繰り越した 2 本目だけが転ぶ。
    return { ok: true, json: async () => (which === 1
      ? {cameras: [], serial_ports: []}
      : {cameras: [], serial_ports: [], camera_error: "uvc: ffmpeg is not installed"}) };
  }
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };
const running = (over) => Object.assign({capturing: "uvc", reconnects: 1, connected: true, paused: false, pauses: 0, switches: 0}, over);

// A を調べ始める。要求はもう出ている (答えはまだ)。
nodes["uvc-device"].value = "A";
loadCameraModes();
await settle();

// 数え直しが始まり、走っている間にもう一度合図が来て繰り越される。
noticeCameras(running());
noticeCameras(running({reconnects: 2}));
await settle();
noticeCameras(running({reconnects: 3}));
await settle();

// 1 本目が数えられる。ここで素性は照らし合わせ済み。
releaseDevices();
await settle();

// **繰り越しの 2 本目が走っている最中に**、数え直しより前の答えが届く。
release();
await settle();

// 2 本目は転ぶ。
releaseDevices();
await settle();

console.log(JSON.stringify({listings, modesFor: modesFor || "", modes: cameraModes.length, sizes: listed["camera-sizes"] || []}));
`
	var got struct {
		Listings int      `json:"listings"`
		ModesFor string   `json:"modesFor"`
		Modes    int      `json:"modes"`
		Sizes    []string `json:"sizes"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.Listings != 2 {
		t.Fatalf("%d listings, want 2 — the signal that arrived mid-run must be carried over", got.Listings)
	}
	if got.Modes != 0 || len(got.Sizes) != 0 {
		t.Errorf("kept %d modes and the sizes %v from an answer that was sent before the cameras were counted, want none — the bridge had already reconciled by then", got.Modes, got.Sizes)
	}
	if got.ModesFor != "" {
		t.Errorf("modesFor = %q, want nothing recorded from an answer that predates the count", got.ModesFor)
	}

	// 上は「この巡が終わったときに、数える前の答えが残っていないこと」を見ています。
	// 世代を進める場所そのものは、そこからは見えません (候補を下ろす側が結果を
	// 覆い隠すため) — 順序で押さえます。数えられた瞬間に進めないと、繰り越しが
	// 走っている間に届いた答えが、いったん今の世代のものとして受理されます。
	body := settingsFunction(t, "async function countCameras() {")
	bumped := strings.Index(body, "countedTimes++")
	looped := strings.Index(body, "} while (listAgain);")
	if bumped < 0 || looped < 0 || bumped > looped {
		t.Error("the generation only advances once the whole run is over, so an answer that lands while the carried-over listing runs is taken as current")
	}
}

// 最初の標本が、最初の列挙より後に繋がったものなら、数え直します。
//
// 最初の状態が読めなかったり遅れたりしている間に初回の列挙が終わり、その一覧に
// カメラがまだ載っていなかった場合、ブリッジは「消えたカメラ」の憶えを残します
// (bridge.forgetModesIfCamerasChanged)。その後で新しい個体が繋がると、最初に届く
// 標本は既に接続済みで、以後は数も動きません。
func TestSettingsPageCountsAgainWhenTheFirstReadingArrivedAfterTheListing(t *testing.T) {
	harness := modesHarness + `
let listings = 0;
const askedModes = globalThis.fetch;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    listings++;
    return { ok: true, json: async () => ({cameras: [], serial_ports: []}) };
  }
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };
const running = (over) => Object.assign({capturing: "uvc", reconnects: 1, connected: true, paused: false, pauses: 0, switches: 0}, over);

// 初回の列挙が終わる。状態はまだ一度も読めていない。
await countCameras();
await settle();
const afterListing = listings;

// そこへ、既に接続済みの標本が最初に届く。この列挙より前に繋がったのか後に
// 繋がったのかは、こちらからは決められない。
const first = noticeCameras(running());
await settle();
const afterFirst = listings;

// 二度は数えない。
noticeCameras(running());
await settle();

console.log(JSON.stringify({afterListing, first, afterFirst, settled: listings}));
release();
`
	var got struct {
		AfterListing int  `json:"afterListing"`
		First        bool `json:"first"`
		AfterFirst   int  `json:"afterFirst"`
		Settled      int  `json:"settled"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.AfterListing != 1 {
		t.Fatalf("%d listings before any reading, want 1", got.AfterListing)
	}
	if !got.First || got.AfterFirst != 2 {
		t.Errorf("counted %d times in total after the first reading, want 2 — that reading may be newer than the listing, and the listing may have been taken while the camera was gone", got.AfterFirst)
	}
	if got.Settled != 2 {
		t.Errorf("counted %d times in total, want 2 — only the first reading is unordered", got.Settled)
	}
}

// 走っている列挙も当てになりません。
//
// Bridge.Devices はカメラを数え終えてからシリアルポートを数えるので、カメラの部分が
// 古い個体や不在を見た後で、差し替えと接続が済んでいることがあります。走っているから
// といって見送ると、その一覧が戻った後は次の遷移まで誰も気づきません。
func TestSettingsPageCarriesTheFirstReadingOverAListingStillRunning(t *testing.T) {
	harness := modesHarness + `
let listings = 0;
let devicePending = [];
const releaseDevices = () => { const waiting = devicePending; devicePending = []; for (const resolve of waiting) resolve(); };
const askedModes = globalThis.fetch;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    listings++;
    await new Promise((resolve) => { devicePending.push(resolve); });
    return { ok: true, json: async () => ({cameras: [], serial_ports: []}) };
  }
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };
const running = (over) => Object.assign({capturing: "uvc", reconnects: 1, connected: true, paused: false, pauses: 0, switches: 0}, over);

// 初回の列挙が走り出す。まだ返らない。
countCameras();
await settle();
const started = listings;

// そこへ、既に接続済みの標本が最初に届く。この列挙のカメラの部分より前に
// 繋がったのか後かは、こちらからは決められない。
const first = noticeCameras(running());
await settle();
const whileRunning = listings;

// 走っているものが終わる。繰り越しがあるので、もう一度数える。
releaseDevices();
await settle();
const afterFirstReturned = listings;

releaseDevices();
await settle();

console.log(JSON.stringify({started, first, whileRunning, afterFirstReturned, settled: listings}));
release();
`
	var got struct {
		Started            int  `json:"started"`
		First              bool `json:"first"`
		WhileRunning       int  `json:"whileRunning"`
		AfterFirstReturned int  `json:"afterFirstReturned"`
		Settled            int  `json:"settled"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.Started != 1 || got.WhileRunning != 1 {
		t.Fatalf("%d listings started and %d while the first was running, want 1 and 1 — the reading must not stack a second listing on top", got.Started, got.WhileRunning)
	}
	if !got.First || got.AfterFirstReturned != 2 {
		t.Errorf("counted %d times once the running listing returned, want 2 — its camera half may predate the reading, so the reading has to be carried over", got.AfterFirstReturned)
	}
	if got.Settled != 2 {
		t.Errorf("counted %d times in total, want 2", got.Settled)
	}
}

// 別のソースへ移って戻ったら、カメラは調べ直します。
//
// 離れている間に差し替えられるかもしれません。憶えを持ち越すと、戻ったときに名前が
// 同じなので「調べ済み」と見なされ、差し替え前の候補をそのまま使わせてしまいます。
// UVC で動いていないならカメラは掴まれていないので、訊けば本当のモードが返ります。
func TestSettingsPageForgetsTheCameraWhileAnotherSourceIsChosen(t *testing.T) {
	harness := modesHarness + `
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };

// A を調べ終える。
nodes["uvc-device"].value = "A";
loadCameraModes();
await settle();
release();
await settle();
const afterFirst = {asked, modesFor: modesFor || ""};

// 別のソースへ移る。ここでは訊かない。
sourceType = "serial";
await loadCameraModes();
await settle();
const afterLeaving = {asked, modesFor: modesFor || ""};

// UVC へ戻る。同じカメラ名だが、離れている間に差し替えられたかもしれない。
sourceType = "uvc";
loadCameraModes();
await settle();
release();
await settle();

console.log(JSON.stringify({afterFirst, afterLeaving, askedAfterReturn: asked, modesForAfterReturn: modesFor || ""}));
`
	var got struct {
		AfterFirst struct {
			Asked    int    `json:"asked"`
			ModesFor string `json:"modesFor"`
		} `json:"afterFirst"`
		AfterLeaving struct {
			Asked    int    `json:"asked"`
			ModesFor string `json:"modesFor"`
		} `json:"afterLeaving"`
		AskedAfterReturn    int    `json:"askedAfterReturn"`
		ModesForAfterReturn string `json:"modesForAfterReturn"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.AfterFirst.Asked != 1 || got.AfterFirst.ModesFor != "A" {
		t.Fatalf("asked %d times and recorded %q on the way in, want 1 and %q", got.AfterFirst.Asked, got.AfterFirst.ModesFor, "A")
	}
	if got.AfterLeaving.Asked != 1 {
		t.Errorf("asked %d times while another source was chosen, want 1 — the camera must be left alone", got.AfterLeaving.Asked)
	}
	if got.AfterLeaving.ModesFor != "" {
		t.Errorf("modesFor = %q after leaving UVC, want it dropped — the camera can be swapped while the page is looking elsewhere", got.AfterLeaving.ModesFor)
	}
	if got.AskedAfterReturn != 2 {
		t.Errorf("asked %d times in total after coming back to UVC, want 2 — the name is the same, so keeping the mark hands back the candidates from before the swap", got.AskedAfterReturn)
	}
	if got.ModesForAfterReturn != "A" {
		t.Errorf("modesFor = %q after coming back, want the camera recorded again", got.ModesForAfterReturn)
	}
}

// 数えられなかったら、合図を待たずに数え直します。
//
// 列挙が一度転ぶと、ブリッジは憶えを捨てず、その一覧を待っているモードの問い合わせも
// 引き下がります。繋がったまま様子が動かなければ次の合図は来ないので、古い候補が
// 読み直すまで残ります。
func TestSettingsPageCountsAgainAfterAListingItCouldNotFinish(t *testing.T) {
	harness := modesHarness + `
let listings = 0;
let failing = true;
const askedModes = globalThis.fetch;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    listings++;
    return { ok: true, json: async () => (failing
      ? {cameras: [], serial_ports: [], camera_error: "uvc: ffmpeg is not installed"}
      : {cameras: [], serial_ports: []}) };
  }
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };
const running = (over) => Object.assign({capturing: "uvc", reconnects: 1, connected: true, paused: false}, over);

let clock = 1000000;
Date.now = () => clock;

// 繋がったまま差し替えを検知して数え直したが、列挙が転んだ。
noticeCameras(running());
noticeCameras(running({reconnects: 2}));
await settle();
const afterFailure = listings;

// 様子は動かない。繋がったままなので、他の合図は来ない。
clock += 29000;
noticeCameras(running({reconnects: 2}));
await settle();
const tooSoon = listings;

// 間隔を越えたら、合図が無くても数え直す。
clock += 2000;
failing = false;
noticeCameras(running({reconnects: 2}));
await settle();
const retried = listings;

// 数えられたので、もう理由は残っていない。
clock += 60000;
noticeCameras(running({reconnects: 2}));
await settle();

console.log(JSON.stringify({afterFailure, tooSoon, retried, settled: listings}));
release();
`
	var got struct {
		AfterFailure int `json:"afterFailure"`
		TooSoon      int `json:"tooSoon"`
		Retried      int `json:"retried"`
		Settled      int `json:"settled"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.AfterFailure != 1 {
		t.Fatalf("counted %d times after the signal, want 1", got.AfterFailure)
	}
	if got.TooSoon != 1 {
		t.Errorf("counted %d times inside the interval, want 1 — a listing takes up to 15s", got.TooSoon)
	}
	if got.Retried != 2 {
		t.Errorf("counted %d times after the interval passed, want 2 — nothing else will signal while it stays connected, so the failure has to be the reason", got.Retried)
	}
	if got.Settled != 2 {
		t.Errorf("counted %d times after it finally succeeded, want 2 — there is no reason left", got.Settled)
	}
}

// 数え直しをまたいだ答えは、名前が合っていても差し替え前のものです。
//
// 問い合わせが返るのを待っている間に差し替えを検知して数え直すと、そこから出る
// 訊き直しは「もう調べている」と見なされて帰ります。その後に届く古い答えを名前だけで
// 採り込むと、差し替え前の候補を調べ済みとして刻み直します。
func TestSettingsPageDropsTheAnswerThatCameFromBeforeTheRecount(t *testing.T) {
	harness := modesHarness + `
const askedModes = globalThis.fetch;
globalThis.fetch = async (url) => {
  if (String(url).startsWith("/api/v1/devices")) {
    return { ok: true, json: async () => ({cameras: [], serial_ports: []}) };
  }
  return askedModes(url);
};
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };

// A を調べ始める。答えはまだ返らない。
nodes["uvc-device"].value = "A";
loadCameraModes();
await settle();
const askedFirst = asked;

// 待っている間に差し替えに気づいて数え直す。ここから出る訊き直しは、走っている
// 問い合わせがあるので帰る。
await countCameras();
await settle();

// そこへ、数え直しより前に立った答えが届く。
release();
await settle();
const staleKept = modesFor || "";
const askedAgain = asked;

// 訊き直しに答える。
release();
await settle();

console.log(JSON.stringify({askedFirst, staleKept, askedAgain, modesFor: modesFor || ""}));
`
	var got struct {
		AskedFirst int    `json:"askedFirst"`
		StaleKept  string `json:"staleKept"`
		AskedAgain int    `json:"askedAgain"`
		ModesFor   string `json:"modesFor"`
	}
	out := runSettingsScript(t, harness)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got.AskedFirst != 1 {
		t.Fatalf("asked for the modes %d times before the recount, want 1", got.AskedFirst)
	}
	if got.StaleKept != "" {
		t.Errorf("modesFor = %q from an answer that was already in flight when the cameras were counted again, want it dropped", got.StaleKept)
	}
	if got.AskedAgain != 2 {
		t.Errorf("asked %d times in total after dropping the stale answer, want 2 — dropping without asking again leaves the candidates empty until the capture moves", got.AskedAgain)
	}
	// 待った先で世代が古くなっていたら、送る前にやめる。送っても答えは捨てられ、
	// 訊き直すことになるので、15 秒の列挙を 2 回直列に走らせるだけ。
	body := settingsFunction(t, "async function loadCameraModes() {")
	sending := strings.Index(body, `fetch("/api/v1/camera-modes`)
	checked := strings.LastIndex(body[:max(sending, 0)], "generation !== countedTimes")
	if sending < 0 || checked < 0 {
		t.Error("nothing checks, before sending, whether the cameras were counted again while this lookup waited — the request is sent only to have its answer thrown away")
	}
	if got.ModesFor != "A" {
		t.Errorf("modesFor = %q after the fresh answer came back, want the camera recorded", got.ModesFor)
	}
}

// 状態の polling がこの合図を拾わなければ、気づく機会がありません。初回の列挙も
// 同じ入口を通さないと、その 15 秒の最中に来た合図が 2 本目を始めます。
func TestSettingsPageWatchesTheCaptureForCameraChanges(t *testing.T) {
	body := settingsFunction(t, "async function pollFFmpeg() {")
	if !strings.Contains(body, "noticeCameras(state)") {
		t.Error("the state polling does not notice that the camera may have been swapped")
	}
	// 送った順に返るとは限りません (setInterval と取得操作の後から重ねて呼ばれます)。
	// 古い標本で今の様子を巻き戻すと、増える一方の数が減って見えて、1 回の遷移から
	// 何度も数え直すことになります — 減ったことも「変わった」だからです。
	dropped := strings.Index(body, "if (mine <= polled) return;")
	noticed := strings.Index(body, "noticeCameras(state)")
	if dropped < 0 || noticed < 0 || dropped > noticed {
		t.Error("a reading that came back late is handed to the camera watch, where the counters going backwards look like a fresh change")
	}
	if !strings.Contains(body, "const mine = ++polls;") {
		t.Error("the state readings are not numbered, so there is no way to tell a late one from a fresh one")
	}
	if strings.Contains(uiSettingsHTML, "devicesListed = loadDevices();\nload();") {
		t.Error("the first device listing bypasses the one that guards against piling up")
	}
	if !strings.Contains(uiSettingsHTML, "countCameras();\nload();") {
		t.Error("the page does not count the cameras on load")
	}
}

// 保存には、土台にした設定の版を添えなければなりません。
//
// 設定は全体で 1 つの値として受け渡されるので、この画面は必ず「読んで、触った葉を
// 重ねて、書く」という往復をします。その 2 つの要求の間に別のクライアントが変更を
// 確定させても、条件を付けなければ誰にも分かりません。後から書いたこちらが黙って
// 消します。
func TestSettingsPageSendsTheVersionItBuiltTheChangeOn(t *testing.T) {
	body := settingsFunction(t, "const save = (cfg, revision) => fetch(")
	if !strings.Contains(body, `"If-Match": revision`) {
		t.Error("the save request does not carry the version the change was built on")
	}
	if !strings.Contains(body, "revision\n    ?") && !strings.Contains(body, "revision ?") {
		t.Error("the save request must still go through when there is no version to send")
	}
}

// 412 は「読んでから書くまでの間に、別のクライアントが確定させた」ということです。
// 黙って上書きしてはいけません — それがこの条件を付けている理由そのものです。
// 何が変わったのかを伝えてから、送り直すかどうかを選ばせます。
func TestSettingsPageAsksBeforeWritingOverSomeoneElsesChange(t *testing.T) {
	body := settingsFunction(t, `el("settings").addEventListener("submit", async (event) => {`)
	if !strings.Contains(body, "412") {
		t.Fatal("the save flow does not notice that the settings moved on")
	}
	if !strings.Contains(body, "confirm(conflictMessage(") {
		t.Error("the save flow overwrites someone else's change without asking")
	}
	// 本体と条件は同じ 1 つの読みから組む。土台を取り直した設定にしながら条件を
	// 画面を描いたときの札にすると、A → B → A と戻る途中で読んだ B を、A の札で
	// 書けてしまう。ユーザーが触ってもいない B の値が確認なしで復活する。
	if !strings.Contains(body, "save(overlay(ground.cfg), ground.revision)") {
		t.Error("the body and the condition come from different reads, so a value the user never saw can be restored under a matching tag")
	}
	if strings.Contains(body, "baselineRevision);") {
		t.Error("a save is conditioned on the version from render time rather than on the settings it is actually built on")
	}
	// 「描いてから動いたか」はこちらで札を見比べる。サーバの 412 には任せられない
	// — まさに A → B → A では札が戻るので 412 が出ない。
	if !strings.Contains(body, "ground.revision !== baselineRevision") {
		t.Error("the page never compares what it drew with what it is about to write on, so a change the user never saw is not a conflict")
	}
	if !strings.Contains(body, "rebase(ground.cfg, dirty, ground.revision)") {
		t.Error("declining the overwrite must leave the page showing the current settings with the user's edits")
	}
	// 訊くのは同じ項目を触っていたときだけ。触っていない項目の変更は、こちらが
	// 押し戻すものではないので、黙って取り直して送り直せば両方の変更が残る。
	if !strings.Contains(body, "clashingLeaves(mine, ground.cfg, dirty)") {
		t.Error("the save flow asks about changes to settings the user never touched")
	}
	// 412 で終わりにしない。取り直してから確認を出している間に、また誰かが書けば
	// 同じことが起きる。1 回で諦めると、その 412 は普通の失敗として扱われ、後の
	// rebase が最新の札とユーザーの古い編集を組み合わせるので、もう一度保存を
	// 押したときに、新しく入った変更を確認なしで消す。
	if !strings.Contains(body, "if (response.status !== 412) break;") {
		t.Error("only the first save can be a conflict, so a change that lands during the retry is overwritten without asking")
	}
	// 比べる相手は、ユーザーが見て承知したところまで進めなければならない。進め
	// ないと、既に承知した同じ変更について何度も訊くことになる。**写しを取る** —
	// 同じオブジェクトを持つと、直後の overlay が比べる相手までユーザーの入力で
	// 塗り替え、承知済みの葉が何度でも競合として挙がる。
	if !strings.Contains(body, "mine = JSON.parse(JSON.stringify(ground.cfg));") {
		t.Error("the next round compares against an object the overlay is about to rewrite, so settings the user already accepted come up again")
	}
	// 触った葉は、競合の判定より先に拾わなければならない。判定は「ユーザーが触った
	// 項目が動いていたか」を見るので、拾う前に判定すると、触った項目が 1 つも無い
	// ことになって、何も訊かずに上書きする — 画面を描いてから最初の読みまでの間に
	// 相手が同じ葉を変えた場合が、まさにそれ。
	touched := strings.Index(body, "dirty.add(i)")
	building := strings.Index(body, "const overlay = (cfg) =>")
	if touched < 0 || building < 0 || touched > building {
		t.Error("the page works out which settings the user touched only while building the body, so the first conflict check sees nothing and overwrites without asking")
	}
}

// 何が変わったかを言うときに比べる相手は、こちらが土台にした設定です。重ねた後の
// ものと比べると、自分の変更まで「相手が変えたもの」として数えます。
func TestSettingsPageNamesOnlyTheSettingsSomeoneElseChanged(t *testing.T) {
	body := settingsFunction(t, "function clashingLeaves(mine, theirs, dirty) {")
	if !strings.Contains(body, "dirty.has(i)") {
		t.Error("settings the user never touched are counted as conflicts")
	}
	if !strings.Contains(body, "field.path.join(\".\")") {
		t.Error("the changed settings are not named")
	}

	// 描いた設定の写しを控えていること。JSON を通すのは、重ねる操作が土台の
	// オブジェクトをその場で書き換えるため。
	kept := settingsFunction(t, "function remember(cfg, revision) {")
	if !strings.Contains(kept, "JSON.parse(JSON.stringify(cfg))") {
		t.Error("the page keeps no copy of what it drew, so it cannot say what someone else changed since")
	}
}
