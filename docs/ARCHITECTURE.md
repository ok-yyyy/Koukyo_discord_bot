# 技術ドキュメント - Koukyo Discord Bot (Go Edition)

## 概要

本Botは `wplace` の監視データを Discord へ通知する Go 実装です。  
Bot 自身が wplace のタイルを定期取得し、テンプレート画像と比較して差分を算出します。

監視・通知・活動集計を分離し、どこかの処理が遅延しても監視ループを止めない設計を採用しています。

## モジュール構成

```
cmd/bot/main.go
  -> internal/monitor        タイル取得 / 差分計算 / 監視状態 / 履歴
  -> internal/notifications  通知判定 / Discord送信 / 日次配信
  -> internal/activity       diff画像ベースのユーザー活動推定
  -> internal/handler        コマンドルーティング
  -> internal/commands       各コマンド実装
  -> internal/embeds         Embed/グラフ/タイムラプス画像生成
  -> internal/wplace         タイル取得 / 画像合成
  -> internal/utils          座標変換 / URL生成 / RateLimiter
```

## 起動シーケンス

1. `cmd/bot/main.go` で設定読込と Discord セッション初期化。
2. 監視用と活動API用の `RateLimiter` をそれぞれ初期化（既定 2 RPS）。
3. `Monitor` 起動（タイル取得ループ）。
4. `Notifier.StartMonitoring()` 起動（通知判定/配信ループ群）。
5. `Tracker.Start()` 起動（diff解析/活動集計）。
6. スラッシュコマンド同期後、Discord 受信開始。

## Monitor 層

主要ファイル: `internal/monitor/monitor.go`, `internal/monitor/reference.go`, `internal/monitor/state.go`

### 役割

- 監視範囲のタイル取得と切り出し
- テンプレートとの差分計算（差分率 / live・diff 画像の生成）
- 監視データ (`MonitorData`) と画像 (`ImageData`) の最新状態保持
- 差分履歴、タイムラプスフレーム、日次サマリの蓄積

### 常駐ループ

- `captureLoop` **5 秒**間隔でタイルを取得して比較（失敗時は指数バックオフ、最大 1 分）

### 差分計算 (`reference.go`)

- テンプレート: `data/template_img/1818-806-989-358.png`（初回取得時に読み込み。失敗した場合は次回再試行）
- 対象はテンプレートの不透明ピクセルのみ
- **差分判定**: RGB 各チャンネルの差の合計が 45 超、またはアルファ差が 15 超
- **diff 画像**: 差分ピクセルを赤で着色（追加監視の差分画像と同じ）
- 算出した diff 画像は `Tracker.EnqueueDiffImage` で ActivityTracker へ連携

### 実装ポイント

- `MonitorState` は `RWMutex` 保護。
- 日次関連は JST キーで保存。

## Notification 層

主要ファイル: `internal/notifications/notifier.go`, `internal/notifications/notifier_monitoring.go`

### 役割

- Tier上昇/下降、完了通知、復帰通知の判定
- small diff（1..10px）専用フロー
- 追加監視/進捗監視の定期比較
- 日次サマリ、日次ランキング、タイムラプス自動配信
- DM速報（ユーザー別・差分率 Tier 変動通知）

### ディスパッチ設計

- 高優先度: `dispatchHigh`（FIFO）
- 低優先度: `dispatchLow`（キー単位 coalescing）
- 飽和時は通知をドロップし、監視ループのブロックを防止

### small diff フロー

- 条件: `DiffPixels` が `1..10`
- Embed ではなくテキストを 1件編集し続ける
- 形式: `- (tileX-tileY-pixelX-pixelY:URL)`
- URL は `/me` 系と同じ高倍率ロジックを利用
- 差分率 0% が 10 分継続すると編集先メッセージ追跡をリセット

### DM速報フロー

主要ファイル: `internal/notifications/notifier_dm.go`

- 監視ループの毎ティックに `CheckAndNotifyDM()` を呼び出し
- `SettingsManager.GetDMEnabledUserIDs()` で有効ユーザー一覧を取得
- 各ユーザーは `dmUserState{lastTier, wasZero}` で Tier 状態を個別管理
- 通知条件: `wasZero→nonzero`（検知）、`nonzero→0%`（完了）、Tier上昇・下降
- 送信: `session.UserChannelCreate` で DM チャンネルを開き `ChannelMessageSend`
- 差分率の 10% 閾値を使用

### 追加監視/進捗監視のエラーポリシー

- 取得失敗、テンプレ解決失敗、比較失敗は Discord 送信しない
- エラーはローカルログのみへ出力
- 回線不良時でも監視ループが通知待ちで停止しない

## Activity 層（断定推定アルゴリズム）

主要ファイル: `internal/activity/tracker.go`

### 前提

`Tracker.UpdateDiffImage` は前回 diff (`oldDiff`) と最新 diff (`newDiff`) を比較し、

- `added`  : 新たに vandal diff になった px 数
- `removed`: 修復されて diff から消えた px 数

を計算します。

### 一般化された断定条件

- vandal 推定: `added >= 2 && removed == 0`
- restore 推定: `added == 0 && removed >= 2`
- 非推定: `added > 0 && removed > 0`（同時増減）

### 推定時の共通挙動

- API呼び出しは 1プローブのみキュー
- 最初に検出したユーザーを `ClaimedPainter` として確定
- 対象px全体を一括クレジット
- 逆方向変化が混ざった時点で推定解除
- TTL 超過時は自動解除

### vandal 推定の内部状態

- `vandalInference` が状態を保持
- `Baseline` は推定開始時点の diff スナップショット
- クレジット対象は `currentDiff - Baseline`

### restore 推定の内部状態

- `restoreInference` が状態を保持
- `Baseline` は推定開始時点の diff スナップショット
- クレジット対象は `Baseline - currentDiff`

### 目的

- 急増/急減局面で API 負荷を削減（レート制限耐性）
- 監視遅延や回線不良時の burst でも集計破綻を抑止

## タイル取得 / 画像合成層

主要ファイル: `internal/wplace/tiles.go`

### 仕様

- HTTP クライアントは接続プール付き
- タイルキャッシュ TTL は2分
- グリッド取得は固定ワーカープール
- `CombineTilesCroppedImage` で必要範囲を切り出し合成

## グラフ / タイムラプス

主要ファイル:

- `internal/embeds/graphs.go`
- `internal/commands/graph.go`
- `internal/embeds/timelapse.go`
- `internal/commands/timelapse.go`

### 時刻基準

- グラフの時刻軸: JST
- タイムラプス表示時刻: JST
- 日次集計キー: JST

### タイムラプス仕様

- 終端フレームを1秒保持し、最終状態を視認しやすくする

## マルチギルド配信

主要ファイル: `internal/notifications/notifier_daily_ranking.go`

- 日次サマリ/ランキング添付画像はギルドごとに個別送信
- 同一バッファ使い回しによる「最初の1ギルドのみ添付」問題を回避

## 永続データ

`data/` 配下に保存:

- `settings.json`
- `user_dm.json` (DM速報の有効ユーザーID一覧)
- `user_activity.json`
- `vandalized_pixels.json`
- `vandal_daily.json`
- `achievements.json`
- `watch_targets.json`
- `progress_targets.json`
- `template_img/*` (メイン監視テンプレート `1818-806-989-358.png` を含む)

## 主要テスト

- `internal/activity/tracker_test.go`
  - 断定ライフサイクル
  - 1プローブキュー
  - 純増/純減での自動arm
  - 増減混在時のリセット
- `internal/notifications/notifier_small_diff_coords_test.go`
- `internal/notifications/notifier_daily_ranking_test.go`
- `internal/embeds/graphs_test.go`
- `internal/monitor/monitor_text_payload_test.go`

---

最終更新: 2026-03-14
