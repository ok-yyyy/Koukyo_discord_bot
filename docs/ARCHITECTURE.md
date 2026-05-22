# 技術ドキュメント - Koukyo Discord Bot (Go Edition)

## 概要

本Botは `wplace` の監視データを Discord へ通知する Go 実装です。  
主経路は WebSocket 監視で、断線時には HTTP poll / standalone 比較へ段階フォールバックします。

監視・通知・活動集計を分離し、どこかの処理が遅延しても監視ループを止めない設計を採用しています。

## モジュール構成

```
cmd/bot/main.go
  -> internal/monitor        WS受信 / 監視状態 / 履歴
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
3. `Monitor` 起動（WS受信ループ群）。
4. `Notifier.StartMonitoring()` 起動（通知判定/配信ループ群）。
5. `Tracker.Start()` 起動（diff解析/活動集計）。
6. スラッシュコマンド同期後、Discord 受信開始。

## Monitor 層

主要ファイル: `internal/monitor/monitor.go`, `internal/monitor/state.go`

### 役割

- WebSocket テキスト/バイナリの取り込み
- 監視データ (`MonitorData`) と画像 (`ImageData`) の最新状態保持
- 差分履歴、タイムラプスフレーム、日次サマリの蓄積
- 無受信監視と再接続、断時フォールバック

### 常駐ループ

- `receiveLoop` 受信本体（切断時は指数バックオフで再接続）
- `pingLoop` WS ping
- `keepaliveLoop` keepalive 送信
- `idleWatchLoop` 無受信監視（長時間停止を検知）
- `pollFallbackLoop` WS断継続時の HTTP 取得

### 実装ポイント

- テキスト受信は `monitorTextPayload` に単一 `json.Unmarshal`。
- `MonitorState` は `RWMutex` 保護。
- 日次関連は JST キーで保存。

## Notification 層

主要ファイル: `internal/notifications/notifier.go`, `internal/notifications/notifier_monitoring.go`

### 役割

- Tier上昇/下降、完了通知、復帰通知の判定
- small diff（1..10px）専用フロー
- 追加監視/進捗監視の定期比較
- 日次サマリ、日次ランキング、タイムラプス自動配信
- DM速報（ユーザー別・加重差分率 Tier 変動通知）

### ディスパッチ設計

- 高優先度: `dispatchHigh`（FIFO）
- 低優先度: `dispatchLow`（キー単位 coalescing）
- 飽和時は通知をドロップし、監視ループのブロックを防止

### small diff フロー

- 条件: `DiffPixels` が `1..10`
- Embed ではなくテキストを 1件編集し続ける
- 形式: `- (tileX-tileY-pixelX-pixelY:URL)`
- URL は `/me` 系と同じ高倍率ロジックを利用
- 省電力モード入退出時に編集先メッセージ追跡をリセット

### Standalone フォールバック

主要ファイル: `internal/notifications/notifier_standalone_fallback.go`

WS が 1 分以上断線した場合（または `MONITOR_FORCE_STANDALONE=1`）に自動起動するポーリングモード。

- ポーリング間隔: **2 秒**（成功後も 2 秒待機、失敗時は指数バックオフ最大 5 分）
- テンプレートで差分 (`DiffPixels`, `DiffPercentage`) を計算して `MonitorData` を更新
- **加重差分**: `data/1818-806-989-358_kiku_only.webp` を `loadTemplateFromDataDir` で読み込み、同 diff 画像から菊のみ差分を算出し `WeightedDiffPercentage` / `ChrysanthemumDiffPixels` に格納
- 算出した diff 画像を `monitor.EnqueueDiffImageToTracker` 経由で ActivityTracker へ連携
- 復帰時はギルドの通知チャンネルへ状態変化を通知

### DM速報フロー

主要ファイル: `internal/notifications/notifier_dm.go`

- 監視ループの毎ティックに `CheckAndNotifyDM()` を呼び出し
- `SettingsManager.GetDMEnabledUserIDs()` で有効ユーザー一覧を取得
- 各ユーザーは `dmUserState{lastTier, wasZero}` で Tier 状態を個別管理
- 通知条件: `wasZero→nonzero`（検知）、`nonzero→0%`（完了）、Tier上昇・下降
- 送信: `session.UserChannelCreate` で DM チャンネルを開き `ChannelMessageSend`
- 常に `weighted` メトリクス・10% 閾値を使用

### Paint回復通知（手動予約）

- `/paint set` によるユーザー指定のタイマー通知
- `internal/commands/paint.go` が `map[string]*time.Timer` でユーザーごとに 1 つの通知を管理
- 指定時間経過後に `Notifier.NotifyPaintRecovery` を呼び出し、DM を送信
- メモリ上でのみ管理（Bot 再起動で予約はリセットされる）

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

- `powerSaveInference` が状態を保持
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
- `template_img/*`
- `1818-806-989-358_kiku_only.webp` (Standalone 加重差分用・菊のみテンプレート)

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
