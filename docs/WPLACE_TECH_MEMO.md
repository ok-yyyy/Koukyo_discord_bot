# Wplace 技術メモ (Koukyo Discord Bot)

最終更新: 2026-02-21

## 1. このメモの範囲

このドキュメントは、このリポジトリでの `wplace` 連携実装を対象にした技術メモです。  
対象は以下です。

- 座標系と URL 仕様
- Wplace データ取得 API
- WebSocket 監視から通知までの処理経路
- ユーザー活動集計と実績付与
- 永続データの意味と運用時の注意点

## 2. 座標系の前提

実装は `internal/utils/coordinator.go` に集約されています。

- Wplace 全体ズーム: `WplaceZoom = 11`
- タイルサイズ: `WplaceTileSize = 1000`
- 1辺のタイル数: `WplaceTilesPerEdge = 2048` (`2^11`)
- ピクセル深掘りリンク用ズーム: `WplaceHighDetailZoom = 21.17`

座標は主に次の3表現を使います。

- 地理座標: `lng, lat`
- タイル+ピクセル: `tileX, tileY, pixelX, pixelY`
- ハイフン文字列: `tileX-tileY-pixelX-pixelY` (例: `1818-806-989-358`)

URL 生成は `BuildWplaceURL` / `BuildWplacePixelURL` / `BuildWplaceHighDetailPixelURL` を使用します。

## 3. Wplace API 利用ポイント

## 3.1 タイル画像 API

`internal/wplace/tiles.go` で利用。

- エンドポイント:  
  `https://backend.wplace.live/tile/{tileX}/{tileY}.png?t={cacheBust}`
- 主用途:
  - `/get` のタイル取得/全域取得
  - `regionmap`
  - 追加監視/進捗監視 (`watch_targets`, `progress_targets`)
- 特徴:
  - タイルキャッシュ TTL は 2分
  - グリッド取得はワーカープール方式
  - `CombineTilesCroppedImage` で必要範囲のみ切り出し可能

## 3.2 ピクセル情報 API

`internal/activity/pixel_api.go` で利用。

- エンドポイント:  
  `https://backend.wplace.live/s0/pixel/{tileX}/{tileY}?x={pixelX}&y={pixelY}`
- 取得対象: `paintedBy` (`id`, `name`, `discordId`, `allianceName`, `picture` など)
- 主用途:
  - diff 発生ピクセルの「誰が塗ったか」推定
  - `/me` 連携フローの認証確認
- 実装上の工夫:
  - API 呼び出しはレートリミッタ経由
  - 429 発生時は `Tracker` 側で指数バックオフ
  - gzip/deflate 応答に対応

## 4. 監視パイプライン (WS -> 集計 -> 通知)

大まかな経路:

1. `internal/monitor/monitor.go`
2. `internal/activity/tracker.go`
3. `internal/notifications/*`

## 4.1 Monitor 層

- WebSocket テキスト:
  - 差分率などのメタデータを `MonitorState` に反映
- WebSocket バイナリ:
  - `type_id=2`: live image
  - `type_id=3`: diff image
  - いずれも 5byte ヘッダ付き (`type_id + payload_size`)
- `diff image` は `Tracker.EnqueueDiffImage` へ流す

WS 障害時のフォールバック:

- 60秒以上 WS 不達で復旧判定
- `MONITOR_POLL_URL` が設定されていれば HTTP Poll に切替
- Poll の再試行は指数バックオフ (最大5分)

## 4.2 Activity (ユーザー活動推定)

`Tracker` は diff 画像の差分から「追加/減少ピクセル」を検出し、必要最小限の pixel API で加害/修復者を割り当てます。

基本ルール:

- 荒らし加算: diff 側に増えたピクセル
- 修復加算: diff から消えたピクセル
- スコア: `activity_score = restored_count - vandal_count`

断定推定ロジック (負荷抑制):

- `added >= 2 && removed == 0` で vandal 推定
- `added == 0 && removed >= 2` で restore 推定
- 混在 (`added > 0 && removed > 0`) は推定解除
- 推定中は 1 プローブ API でまとめてクレジット

## 4.3 Notification 層

- 監視通知 (差分率/Tier/復帰/完了)
- small diff (10px以下) は 1メッセージを編集し続ける運用
- 追加監視/進捗監視はテンプレ画像比較で通知

Wplace 直結の主要補助:

- `target_common.go` でテンプレ比較結果を生成
- 画像の中心座標から `BuildWplaceURL` を作成
- `/get fullsize` 互換の `fullsize` 文字列を埋め込む

## 5. `user_activity.json` と実績の関係

## 5.1 `user_activity.json`

主なフィールド (`internal/activity/tracker.go`):

- `id` (Wplace user id)
- `name`, `allianceName`
- `discord`, `discord_id`
- `last_seen`
- `vandal_count`, `restored_count`, `activity_score`
- `daily_vandal_counts`, `daily_restored_counts`, `daily_activity_scores`

保存:

- Dirty フラグ方式で定期フラッシュ
- `utils.WriteFileAtomic` で原子的書き込み
- ユーザーデータは半永久的に保持 (`activityRetentionDays = 36500`)
- 実績評価 (`achievements.json`)

`internal/notifications/notifier_achievements.go` が 1分ごとに評価します。

流れ:

1. `achievement_rules.json` をロード
2. `user_activity.json` を評価
3. `achievements.json` に保存
4. 初回はベースライン同期 (通知抑止)、2回目以降は通知

ID 連携仕様:

- Discord 連携前ユーザー (`wplace:<id>` キー) にも実績は付与される
- 連携後は Discord ID 優先キーに統合
- 分断レコードは `Store` 側でマージして扱う

## 6. `/me` 連携フローの要点

`internal/commands/me_link.go` の仕様。

- 固定タイル上のランダムピクセルを 1ユーザーに払い出し
- 1分以内に色変更してもらい、pixel API で変化を検出
- 成功時に `user_activity.json` の `discord_id` / `discord` を更新
- 既に別 Discord に紐づいている Wplace ID は拒否

この仕組みで、Wplace 側の実際の塗り操作を使って本人性を確認します。

## 7. レート制御と安定化ポイント

- 外部 API は `RateLimiter` 経由 (既定 2 RPS)
- pixel API は 429 を検知して指数バックオフ
- タイル取得は並列数を制限
- 通知系は監視ループをブロックしないディスパッチ設計

## 8. 運用時によく見る確認ポイント

- Wplace 監視が止まる:
  - `WEBSOCKET_URL` 到達性
  - `MONITOR_POLL_URL` のフォールバック可否
- 活動集計が増えない:
  - diff 画像が type_id=3 で流れているか
  - pixel API 429 が継続していないか
- 実績が出ない:
  - `achievement_rules.json` の条件
  - `achievementchannel` 設定
  - 初回評価は通知抑止仕様である点

## 9. 関連コード索引

- 座標/URL: `internal/utils/coordinator.go`, `internal/utils/map_zoom.go`
- タイル取得: `internal/wplace/tiles.go`
- WS監視: `internal/monitor/monitor.go`
- 活動集計: `internal/activity/tracker.go`, `internal/activity/pixel_api.go`
- 実績評価: `internal/notifications/notifier_achievements.go`, `internal/achievements/store.go`
- 連携フロー: `internal/commands/me_link.go`
- 取得系コマンド: `internal/commands/get_command.go`, `internal/commands/regionmap_command.go`, `internal/commands/convert.go`
