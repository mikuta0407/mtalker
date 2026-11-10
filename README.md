# mtalker

`open_jtalk` を使って Discord のテキストチャンネル投稿を読み上げる Go 製 TTS Bot です。

`/ttsjoin` を実行すると、コマンド実行者が参加しているボイスチャンネルへ接続し、そのコマンドを実行したテキストチャンネルの新規投稿を順番に読み上げます。`/ttsdisconnect` で切断します。ギルド単位で 1 セッションを持ち、複数ギルドで同時運用できます。

## 主な機能

- `/ttsjoin` でコマンド実行者の現在のボイスチャンネルへ接続
- `/ttsdisconnect` で現在の読み上げセッションを切断
- 読み上げ対象は `/ttsjoin` を実行したテキストチャンネルのみ
- URL を `URL` に置換
- 改行を削除して 1 行化
- 140 文字超の本文は `以下省略` に置換
- `open_jtalk` が生成した WAV を Discord 向け Opus フレームへ変換して再生
- ギルドごとのキューで逐次再生し、音声が重ならないように制御
- 一時生成した txt / wav ファイルを自動削除

## 動作概要

1. 起動時に必須環境変数、`open_jtalk` の存在、辞書パス、voice パスを検証します。
2. ユーザーがテキストチャンネルで `/ttsjoin` を実行します。
3. Bot はコマンド実行者が参加しているボイスチャンネルへ接続します。
4. 同じテキストチャンネルの新規投稿を監視し、本文を正規化して一時 txt ファイルを作成します。
5. `open_jtalk -x <DICPATH> -m <VOICEPATH> -ow <wavファイル>` を実行して WAV を生成します。
6. WAV を 48kHz / stereo / 20ms 単位の Opus フレームへ変換し、Discord VC に送信します。
7. `/ttsdisconnect` または接続終了時にセッションを破棄します。

## Docker で起動する

必要なもの:

- Docker
- Docker Compose v2 (`docker compose`)
- Discord Bot トークン

1. このリポジトリをcloneします
2. [docker-compose.yaml](docker-compose.yaml) の `DISCORD_TOKEN` を自分の Bot トークンへ書き換えます
3. 次を実行します

```bash
docker compose up -d
```

初回はイメージビルドが走るため時間がかかります。Docker イメージ内では以下を自動で準備します。

- `open_jtalk`
- `open-jtalk-mecab-naist-jdic`
- `libdave` のソースビルド
- `tohoku-f01-neutral.htsvoice` の取得

`DICPATH` と `VOICEPATH` は [docker-compose.yaml](docker-compose.yaml) にコンテナ内の既定値を設定済みです。独自 voice を使いたい場合だけ変更してください。

ログ確認:

```bash
docker compose logs -f mtalker
```

停止:

```bash
docker compose down
```

## ローカル実行の前提条件

- Go 1.27 以上
- `open_jtalk` コマンドが `PATH` 上に存在すること
- `open_jtalk` 用の辞書ディレクトリ
- `.htsvoice` ファイル
- `github.com/disgoorg/godave/golibdave` が利用するネイティブライブラリ `libdave`
- Discord Bot アプリケーション

## 手動セットアップ

### 1. `open_jtalk` を準備する

#### Ubuntu / Debian 系

```bash
sudo apt-get update
sudo apt-get install -y open-jtalk open-jtalk-mecab-naist-jdic
```

辞書パスの例:

```bash
export DICPATH="/var/lib/mecab/dic/open-jtalk/naist-jdic"
```

voice ファイルは別途用意してください。たとえば、[tohoku-f01-neutral.htsvoice](https://github.com/icn-lab/htsvoice-tohoku-f01/blob/master/tohoku-f01-neutral.htsvoice) を利用できます。

```bash
export VOICEPATH="$HOME/voices/tohoku-f01-neutral.htsvoice"
```

#### `open_jtalk` 動作確認

起動前に、単体で WAV を生成できることを確認してください。

```bash
echo 'こんにちは' | open_jtalk -x "$DICPATH" -m "$VOICEPATH" -ow /tmp/test.wav
```

`/tmp/test.wav` が生成され、再生できれば準備完了です。

### 2. `libdave` を準備する

Discord 音声接続のために `godave` / `golibdave` を使用しており、ネイティブライブラリ `libdave` が必要です。

公式 README:

- [disgoorg/godave README](https://github.com/disgoorg/godave/blob/master/README.md)

公式スクリプトの利用例:

```bash
curl -fsSL -o libdave_install.sh https://raw.githubusercontent.com/disgoorg/godave/refs/heads/master/scripts/libdave_install.sh
chmod +x libdave_install.sh
./libdave_install.sh v1.1.0
```

スクリプトは利用可能であれば事前ビルド済みバイナリを取得し、必要に応じてソースビルドへフォールバックします。`go build` や `go run` で `pkg-config` 関連のエラーが出る場合は、少なくとも次をシェルへ通してください。

```bash
export PKG_CONFIG_PATH="$HOME/.local/lib/pkgconfig:$PKG_CONFIG_PATH"
```

#### macOS (Homebrew)

Homebrew 版の `open-jtalk` が利用できます。

```bash
brew install open-jtalk
```

Homebrew で導入した場合は、辞書とサンプル voice を次のように参照できます。

```bash
export DICPATH="$(brew --prefix open-jtalk)/dic"
export VOICEPATH="$(brew --prefix open-jtalk)/voice/mei/mei_normal.htsvoice"
```

### 3. Discord Bot を準備する

#### OAuth2 Scope

- `bot`
- `applications.commands`

#### Developer Portal で有効化するもの

- `MESSAGE CONTENT INTENT`

#### Bot に必要な権限の目安

- OAuth2の設定
  - bot
    - テキストの権限
      - 「メッセージを送る」
    - 音声の権限
      - 接続
      - 発言

## 環境変数

| 変数名 | 必須 | 説明 |
| --- | --- | --- |
| `DISCORD_TOKEN` | 必須 | Discord Bot トークン |
| `DICPATH` | 必須 | `open_jtalk` が利用する辞書ディレクトリ |
| `VOICEPATH` | 必須 | 読み上げに使う `.htsvoice` ファイルのパス |

### macOS 向け設定例

```bash
export DISCORD_TOKEN="xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
export DICPATH="$(brew --prefix open-jtalk)/dic"
export VOICEPATH="$(brew --prefix open-jtalk)/voice/mei/mei_normal.htsvoice"
```

### Linux 向け設定例

```bash
export DISCORD_TOKEN="xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
export DICPATH="/var/lib/mecab/dic/open-jtalk/naist-jdic"
export VOICEPATH="$HOME/voices/tohoku-f01-neutral.htsvoice"
```

## 起動方法

```bash
go run .
```

ビルドする場合:

```bash
go build ./...
```

起動時に以下を満たさない場合は即座に終了します。

- 必須環境変数が未設定
- `open_jtalk` が `PATH` 上にない
- `DICPATH` が存在しない
- `VOICEPATH` が存在しない

## コマンド

| コマンド | 説明 |
| --- | --- |
| `/ttsjoin` | コマンド実行者が今いるボイスチャンネルへ接続し、コマンドを実行したテキストチャンネルの監視を開始します |
| `/ttsdisconnect` | 現在のギルドの読み上げセッションを停止し、ボイス接続を切断します |

### `/ttsjoin` の挙動

- サーバー内でのみ利用できます
- 引数はありません
- コマンド実行者が VC にいない場合はエラーになります
- 同一ギルドで既にセッションがある場合は重複起動できません
- 成功すると、そのギルドでは 1 つのテキストチャンネル + 1 つの VC の組で読み上げが開始されます

## 読み上げ仕様

- 監視対象はアクティブセッションのテキストチャンネルだけです
- Bot 自身の投稿、Webhook 投稿、システムメッセージ、空文字投稿は読み上げません
- 読み上げ対象は `Message.Content` のみです
	- 添付ファイル
	- Embed
	- スタンプ
	- リアクション
	は読み上げ対象外です
- テキスト正規化ルールは以下の通りです
	- URL を `URL` に置換
	- 改行をすべて削除
	- 前後の空白をトリム
	- 140 文字を超える場合は本文全体を `以下省略` に置換
- 一時 txt ファイルは `os.TempDir()` 配下に `<channel>.<unixnano>.*.txt` 形式で作成されます
- 生成 wav は `voice_<unixnano>_*.wav` 形式で作成され、再生後に削除されます
- ギルドごとにキューを持ち、順番に 1 件ずつ再生します
- キュー容量のデフォルトは 32 件です

## 注意点

- `MESSAGE CONTENT INTENT` が無効だと本文を取得できず、読み上げが機能しません
- グローバルスラッシュコマンドの反映には時間がかかることがあります
- 読み上げ対象は本文のみです。添付や Embed の読み上げには未対応です

## ライセンス

このリポジトリ内のソースコードは [MIT License](LICENSE) です。

ただし、Docker ビルド時に取得する `tohoku-f01-neutral.htsvoice` などの外部コンポーネントには、それぞれ個別のライセンスが適用されます。利用時は配布元の条件も確認してください。

