# `capture` コマンド オプション一覧

`capture start` は管理シェルからリモート端末の画面や Web カメラ映像を取得するためのコマンドです。以下に使用可能なフラグと挙動をまとめます。

| フラグ | 省略形 | 説明 | 既定値 / 備考 |
|--------|--------|------|----------------|
| `--open` | `-o` | 取得開始と同時に `ffplay` を起動しライブビューを表示します。 | 既定: 無効 |
| `--save <path>` | なし | 取得したストリームを指定ファイルへ保存します。パスは環境依存の `~` 展開に対応。 | 既定: `./capture-<user>-<timestamp>.ts` (保存先未指定かつ `--no-save` を使わない場合) |
| `--encoder <name>` | なし | FFmpeg エンコーダを指定します。 | 既定: `libx264` |
| `--format <mime>` | なし | 送信コンテナ形式を指定します。(例: `video/mp2t`, `video/mp4`, `video/webm`) | 既定: `video/mp2t` |
| `--framerate <fps>` | なし | 取得フレームレートを指定します。 | 既定: `24` |
| `--bitrate <kbps>` | なし | 映像ビットレートを kbps 単位で指定します。 | 既定: `1500` |
| `--width <px>` | なし | 出力映像の幅を指定します。 | 既定: `1280` |
| `--height <px>` | なし | 出力映像の高さを指定します。 | 既定: `720` |
| `--keyframe <frames>` | なし | キーフレーム間隔をフレーム数で指定します。 | 既定: `48` |
| `--no-save` | なし | 保存せずライブビューや転送のみを行います。 | 既定: 無効 |
| `--source <screen\|webcam>` | なし | キャプチャ対象を切り替えます。`screen` は従来どおりの画面共有、`webcam` で Web カメラ入力に切り替え。 | 既定: `screen` |
| `--webcam` | なし | `--source webcam` のショートカット。 | 既定: 無効 |
| `--device <identifier>` | なし | 入力デバイスや入力ソースを明示します。画面の場合は `:0.0` やモニター ID、Web カメラの場合は `/dev/video0` や `video="USB Camera"` など。Windows では必須。 | 既定: 画面は OS ごとの標準、Web カメラは OS ごとの標準デバイス |

## 代表的な使用例

### 画面共有（従来どおり）
```bash
capture start --open
```
- 24fps / 1280x720 / 1.5Mbps で取得しつつ、`ffplay` でライブ表示します。

### 指定ディスプレイを録画
```bash
capture start --source screen --device :1.0 --save ~/captures/monitor1.ts
```
- X11 上の `:1.0` ディスプレイを録画しローカルに保存、ライブビューは開きません。

### Web カメラ配信
```bash
capture start --webcam --device /dev/video2 --open --no-save
```
- Linux で `/dev/video2` の Web カメラ映像を取得しライブ表示。保存は行いません。
- フレームレートを指定しない場合、デバイスが提供する既定値で動作します。

Windows の場合:
```bash
capture start --webcam --device "USB Live Camera" --format video/mp4
```
- DirectShow の `video=USB Live Camera` を入力とし、MP4 ストリームを生成します。

## デバイス一覧の確認

- **Windows**: `ffmpeg -hide_banner -f dshow -list_devices true -i dummy`
- **macOS**: `ffmpeg -hide_banner -f avfoundation -list_devices true -i ""`
- **Linux**: `v4l2-ctl --list-devices` もしくは `ls /dev/video*`

Windows では必ずデバイス名を指定する必要があります。スペースを含む場合は `--device "USB Live Camera"` のように引用符で囲んでください。

## 環境変数
- `MODERNRAT_CAPTURE_SOURCE` … 画面キャプチャ時の入力ソース上書き。
- `MODERNRAT_WEBCAM_DEVICE` … Web カメラキャプチャ時のデバイスパス/名称上書き。

これらを設定すると CLI フラグを明示しなくても既定値を変更できます。

## 備考
- Web カメラ入力では OS に応じて FFmpeg の `v4l2`(Linux) / `dshow`(Windows) / `avfoundation`(macOS) を利用します。必要なドライバや権限が揃っていることを確認してください。
- `--open` を用いる場合、`ffplay` バイナリが同梱されているか PATH に存在する必要があります。
- 保存ディレクトリが存在しない場合は自動で作成します。
