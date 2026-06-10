# Koukyo Discord Bot (Go Edition)

Wplace 監視を行う Discord Bot の Go 実装です。WebSocket の差分監視・通知・ユーザー活動集計・画像生成をまとめて提供します。

## クイックスタート

### 1) 環境変数（推奨: Docker env_file）

ルート直下に secrets を置く想定

中身:

```env
DISCORD_TOKEN=your_discord_token_here
WEBSOCKET_URL=ws://monitor:8000/ws
MONITOR_POLL_URL=https://example.com/monitor/status
MONITOR_FORCE_STANDALONE=0
MONITOR_STANDALONE_TARGET_ID=
MONITOR_STANDALONE_ORIGIN=1818-806-989-358
MONITOR_STANDALONE_TEMPLATE=1818-806-989-358.png
POWER_SAVE_MODE=0
```

`docker-compose.yml` からは以下のように参照します:

```yaml
services:
  discord-bot:
    env_file:
      - .env
```

### 2) Docker 起動

```bash
docker compose up --build
```

### 3) ローカル起動

```bash
go run ./cmd/bot
```

## 主な機能

- WebSocket での差分監視（差分率/加重差分率、画像データ）
- 差分通知（Tier 制、0%復帰/完了通知、ロールメンション対応）
- 差分通知に同時検出ユーザーの内訳表示（`user#id | xxpx`、上位5件）
- 小規模差分モード（10px以下）: 1つのテキスト通知を更新し続け、差分座標を高倍率URL付きで表示
- **DM速報** (`/dm on`): 加重差分率10%以上のTier変動をユーザーへDM通知。`/dm off` で解除
- **Paint回復通知**: `/paint set` でPaint全回復までの時間を計算し、完了時にDMで通知予約が可能。`/paint cancel` で解除
- 断定推定（vandal/restore 両対応）: 純増/純減のみの変化時に最初の検出ユーザーへ高確率帰属
- サーバー別設定パネル（`/settings`）
- ユーザー活動の追跡/可視化（荒らし/修復のスコア・履歴）
- 画像生成（/now の結合画像、グラフ/ヒートマップ/タイムラプス）
- 地図/タイル取得ユーティリティ（`/get`、`/regionmap`）
- 追加監視（`watch_targets.json`）と進捗監視（`progress_targets.json`）
- WebSocket 断時のフォールバック（HTTP Poll → Standalone 2秒間隔ポーリング）
  - Standalone 時は `data/1818-806-989-358_kiku_only.webp` で菊のみ加重差分を算出
- 外部 API 向けのレートリミッター（既定 2 RPS）

## コマンド一覧

テキストコマンドは `!` プレフィックス、スラッシュコマンドは `/` で利用できます（一部はスラッシュ専用）。
起動時に自動でスラッシュコマンドを同期します。

### 監視・通知
- `now` - 現在の監視状況を表示（Wplace URL / fullsize導線つき）
- `graph` - 差分率の時系列グラフ（期間指定可）
- `predict` - 修復速度から完全修復までの推定時間を表示
- `timelapse` - 差分率 30%→0.2% のタイムラプス（GIF）
- `heatmap` - 最近の変化量ヒートマップ
- `dm` - 自分へのDM速報を有効/無効にする（加重差分率10%以上で通知）
- `explanation` - 監視項目や用語の解説を表示（スラッシュ専用）
- `settings` - 通知/閾値などの設定パネル（管理者向け）
- `notification` - 荒らし/修復ユーザー通知チャンネル設定（管理者向け）
- `progresschannel` - ピクセルアート進捗通知チャンネル設定（管理者向け）
- `status` - Bot 自体の稼働状況（メモリ、稼働時間など）

### ユーザー活動
- `me` - 自分の活動カード表示（Wplace 連携フローあり）
- `achievements` - 自分の実績一覧を表示
- `achievementchannel` - 実績通知チャンネルを設定（管理者向け）
- `useractivity` - ユーザー活動の検索/詳細表示（スラッシュ専用、詳細で実績も表示）
- `fixuser` - 修復ユーザー一覧（ランキング/最近、score/absolute）
- `grfuser` - 荒らしユーザー一覧（ランキング/最近、score/absolute）

### 地図・取得系
- `get` - タイル/Region/フルサイズ画像取得（スラッシュ専用）
- `regionmap` - 地域の Region 配置マップ（スラッシュ専用）
- `convert` - 座標変換（経度緯度 ⇄ ピクセル）

### 便利コマンド
- `help` - コマンド一覧
- `info` - Bot 情報
- `ping` - 疎通確認
- `proxy` - `[Golden Proxy]{display} (@username / userID)` で代理投稿（Webhook）
- `proxydelete` - 代理投稿メッセージを削除（管理者向け）
- `time` - 時刻表示/時差変換
- `paint` - Paint 回復時間の計算・通知予約（スラッシュ専用）
  - `/paint set`: 現在値と上限値を入力し、全回復までの時間を計算。`notify: on` で完了時にDM通知。
  - `/paint cancel`: 予約されている通知をキャンセル。

※ `proxy` はチャンネルごとにWebhookを再利用し、Webhook由来の投稿者IDが毎回変わらないようにしています。

※ `graph` / `timelapse` / `heatmap` は WebSocket 監視が有効なときのみ利用できます。

## 追加監視 / 進捗監視

### 追加監視（荒らし検知）
- 設定: `data/watch_targets.json`
- 画像: `data/template_img/`
- 手動取得: `!{id}`

### 進捗監視（制作向け）
- 設定: `data/progress_targets.json`
- 画像: `data/template_img/`
- チャンネル設定: `/progresschannel act:on`
- 手動取得: `!{id}`

JSON 形式は共通です:
```json
{
  "targets": [
    {
      "id": "koukyo-main",
      "label": "Koukyo Main",
      "origin": "1818-806-989-358",
      "template": "koukyo_main.png",
      "interval_seconds": 30
    }
  ]
}
```

### 通知ポリシー（重要）

- 追加監視 / 進捗監視の「取得失敗」「テンプレート解決失敗」などは、Discord チャンネルへは送信せずローカルログのみに出力します。
- 誤検知や回線不良で監視ループに影響が出ないよう、通知処理は非同期ディスパッチで監視ループと分離しています。

### 小規模差分（10px以下）の挙動

- 10px 以下の差分は Embed ではなくテキスト通知を更新（編集）して運用します。
- 差分行は `- (tileX-tileY-pixelX-pixelY:URL)` 形式で、高倍率URL（`BuildWplaceHighDetailPixelURL`）を出力します。
- 省電力モードの入退出時は small diff の編集先メッセージ追跡をリセットし、古いメッセージ誤編集を防止します。
- 差分が 10px を超えると large diff 通知に遷移し、スナップショット付き通知を送信します。
- 通知Embedには、現在差分に含まれるユーザー内訳（`user#id | xxpx`）を上位5件まで表示します。

## 断定推定ロジック（重要）

`internal/activity/tracker.go` では、連続2回の diff 画像比較から `added` / `removed` を求め、以下の条件で断定推定を自動適用します。

- vandal 推定開始: `added >= 2 && removed == 0`
- restore 推定開始: `added == 0 && removed >= 2`
- 推定しない: `added > 0 && removed > 0`（増減が混在）

推定中の挙動:

- API は「1プローブのみ」実行（残りは推定割当）
- 最初に検出したユーザーを対象変化へ一括クレジット
- 逆方向の変化が混ざった時点で推定を即解除し通常フローへ復帰
- 単発ノイズ抑制のため、閾値は 2px 以上

この仕様により、回線不良時の API 過負荷とレートリミット衝突を抑えつつ、急激な変化局面での帰属精度を保ちます。

## 設定

必須/任意の環境変数:

- `DISCORD_TOKEN` (必須)
- `WEBSOCKET_URL` (任意: 未指定の場合は監視機能が無効)
- `MONITOR_POLL_URL` (任意: WebSocket が1分以上切断された際のHTTPフォールバック取得先)
- `MONITOR_FORCE_STANDALONE` (任意: `1` で起動時から常にスタンドアローン監視モード。WSサーバーが停止している場合に有効)
- `MONITOR_STANDALONE_TARGET_ID` (任意: WS断時/強制スタンドアローン時の自前監視で使う watch target ID。指定時のみ watch_targets を参照)
- `MONITOR_STANDALONE_ORIGIN` (任意: watch target が解決できない場合のフォールバック座標)
- `MONITOR_STANDALONE_TEMPLATE` (任意: watch target が解決できない場合のフォールバックテンプレート。既定: `1818-806-989-358.png`)
- `POWER_SAVE_MODE` (任意: `1` で起動時に省電力モード)

## 時刻基準

- グラフ (`graph`) は JST 軸で表示します。
- タイムラプス (`timelapse`) の開始/終了時刻は JST で表示します。
- 日次サマリ/日次ランキング集計は JST 日付で処理します。
- Paint回復通知 (`paint`) はユーザーが指定したタイムゾーン（既定: JST）で時刻を表示します。

## 日次/タイムラプス配信

- 日次サマリの結合画像はマルチギルド環境でも各ギルドへ個別添付されます（単一添付欠落バグ修正済み）。
- タイムラプス自動送信も同様にギルド単位で安定配信されます。
- タイムラプス GIF は最終フレームを 1 秒保持し、終端の視認性を改善しています。

データは `data/` に保存されます（Docker 利用時は `/app/data`）。

- `data/settings.json`
- `data/user_dm.json` (DM速報の有効ユーザー一覧)
- `data/achievement_rules.json` (実績付与ルール定義)
- `data/user_activity.json` (ユーザーの活動統計データを半永久的に保持)
- `data/vandalized_pixels.json`
- `data/vandal_daily.json`
- `data/achievements.json`
- `data/watch_targets.json` (追加監視ターゲット定義)
- `data/progress_targets.json` (進捗監視ターゲット定義)
- `data/template_img/` (監視用テンプレート画像)
- `data/1818-806-989-358_kiku_only.webp` (Standalone 加重差分用・菊のみテンプレート)

## 実績ルールJSON

実績の付与条件は `data/achievement_rules.json` で定義できます。
Botは定期的（1分ごと）に `user_activity.json` を評価し、条件達成時に `achievements.json` へ付与します。
`/achievementchannel` が設定されているギルドには獲得通知を送信します。
起動直後の初回評価はベースライン同期として扱われ、通知は抑止されます（保存のみ）。
実績通知の表示名はゲーム内ユーザー名を優先します。Discord未連携ユーザーでも実績付与対象です。

### ルール例

```json
{
  "version": 1,
  "rules": [
    {
      "id": "restorer_50",
      "name": "Restorer 50",
      "description": "修復数が50回に到達",
      "conditions": {
        "restored_count_gte": 50
      }
    }
  ]
}
```

### conditions キー解説

すべての条件は **AND 条件** です（書いた条件をすべて満たした時に付与）。

| key | 型 | 意味 |
|---|---|---|
| `vandal_count_gte` | number | 累計荒らし数がこの値以上 |
| `restored_count_gte` | number | 累計修復数がこの値以上 |
| `activity_score_gte` | number | 活動スコア（`修復数 - 荒らし数`）がこの値以上 |
| `activity_score_lte` | number | 活動スコア（`修復数 - 荒らし数`）がこの値以下 |
| `total_actions_gte` | number | 総アクション数（`修復数 + 荒らし数`）がこの値以上 |
| `max_daily_vandal_gte` | number | 1日あたり荒らし数の最大値がこの値以上 |
| `max_daily_restored_gte` | number | 1日あたり修復数の最大値がこの値以上 |
| `active_days_gte` | number | 活動が1件以上あった日数（荒らし/修復どちらでも可）がこの値以上 |
| `inactive_days_gte` | number | 最終観測からの経過日数がこの値以上 |
| `discord_linked_required` | boolean | `true`: Discord連携済みユーザーのみ対象 / `false`: 未連携のみ対象 |

`enabled: false` を指定すると、そのルールは評価対象から外れます。
`inactive_days_gte` は `user_activity.json` の `last_seen` を基準に判定されます。

### conditions 例

```json
{
  "id": "elite_restorer",
  "name": "Elite Restorer",
  "description": "累計修復300回以上かつスコア+200以上",
  "conditions": {
    "restored_count_gte": 300,
    "activity_score_gte": 200,
    "discord_linked_required": true
  }
}
```

## Discord 側の設定

Bot に以下の Intents を許可してください:

- `Guilds`
- `Guild Messages`
- `Message Content`

※ メッセージコマンドを使わない場合でも、`Message Content` が無効だと `!` コマンドは反応しません。

## 実行方法

ローカル実行:
```bash
go run ./cmd/bot
```

ビルド:
```bash
go build -o bot.exe ./cmd/bot
```

Docker:
```bash
docker compose up --build
```

## パフォーマンス / 安定性メモ

- レートリミッター既定値は 2 RPS（`cmd/bot/main.go` で `NewRateLimiter(2)`）。
- Discord REST クライアントに 15 秒タイムアウトを設定し、通知更新で監視ループが詰まるリスクを低減。
- タイル一括取得はワーカープール方式で実行し、過剰な goroutine 生成を回避。
- small diff 座標抽出はキャッシュし、同一 diff 画像の再デコードを抑制。
- WebSocket テキストメッセージ解析は単一 Unmarshal に統一してオーバーヘッドを削減。
- 断定推定は 1プローブ API 戦略で実装し、急増/急減局面の API コストを大幅に抑制。

## トラブルシュート

- スラッシュコマンドが出てこない
  -> Bot の再起動後に同期されます。権限不足や API エラーがある場合はログを確認してください。

- 監視が動かない
  -> `WEBSOCKET_URL` の設定と接続先の到達性を確認してください。

- 通知が来ない
  -> `/settings` で通知チャンネルを設定し、`auto_notify` が有効か確認してください。
  -> 進捗通知は `/progresschannel act:on` が必要です。

- DM速報が届かない
  -> `/dm on` で有効化してください。加重差分率が取得できない場合（WS未接続かつ kiku テンプレート未配置）は送信されません。

## GitHub 用メモ

- 設定ファイルは `data/` に置きます（Docker では `/app/data`）
- `data/template_img/` 配下は監視テンプレ画像を保存します

## 移植元

wplace-koukyo-bot (Python)
