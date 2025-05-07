package main

import (
	"encoding/json"
	"fmt"
	"io" // io.ReadAll を使用するために追加
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort" // ソート処理のために追加
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

// httpPort はHTTPサーバーがリッスンするポートです。KoyebのPORT環境変数を優先します。
const httpPortEnvVar = "PORT"
const defaultHttpPort = "8080"

const (
	riotAccountAPIBaseURL = "https://asia.api.riotgames.com" // PUUID取得用
	riotMatchAPIBaseURL   = "https://asia.api.riotgames.com" // Match-V5 API用 (地域エンドポイント)
	riotLolAPIBaseURL     = "https://jp1.api.riotgames.com"  // LoL関連情報取得用 (プラットフォームエンドポイント)
	// Riot APIのレート制限を考慮
	apiRequestDelay   = 1200 * time.Millisecond
	rankedSoloQueueID = 420 // RANKED_SOLO_5x5 のキューID
)

// AccountDTO Riot Account APIから返されるアカウント情報
type AccountDTO struct {
	PUUID    string `json:"puuid"`
	GameName string `json:"gameName"`
	TagLine  string `json:"tagLine"`
}

// SummonerDTO Riot LoL APIから返されるサモナー情報
type SummonerDTO struct {
	ID        string `json:"id"` // Encrypted Summoner ID
	AccountID string `json:"accountId"`
	PUUID     string `json:"puuid"`
	Name      string `json:"name"`
}

// LeagueEntryDTO Riot LoL APIから返されるランク情報
type LeagueEntryDTO struct {
	LeagueID     string `json:"leagueId"`
	SummonerID   string `json:"summonerId"`
	SummonerName string `json:"summonerName"` // APIから返されるサモナー名
	QueueType    string `json:"queueType"`
	Tier         string `json:"tier"`
	Rank         string `json:"rank"`
	LeaguePoints int    `json:"leaguePoints"`
	Wins         int    `json:"wins"`
	Losses       int    `json:"losses"`
	HotStreak    bool   `json:"hotStreak"`
	Veteran      bool   `json:"veteran"`
	FreshBlood   bool   `json:"freshBlood"`
	Inactive     bool   `json:"inactive"`
}

// PlayerRankInfo ソートと比較のためにランク情報を保持する構造体
type PlayerRankInfo struct {
	RiotID       string // ユーザーが指定したRiot ID (GameName#TagLine)
	Tier         string
	Rank         string
	LeaguePoints int
	TierValue    int // ソート用のティア数値
	RankValue    int // ソート用のランク数値
}

// MatchDTO Riot Match-V5 APIから返される試合詳細情報 (必要な部分のみ抜粋)
type MatchDTO struct {
	Metadata struct {
		MatchID      string   `json:"matchId"`
		Participants []string `json:"participants"` // PUUIDのリスト
	} `json:"metadata"`
	Info struct {
		GameCreation     int64            `json:"gameCreation"`     // 試合開始時刻 (Unix milliseconds)
		GameDuration     int64            `json:"gameDuration"`     // 試合時間 (seconds)
		GameEndTimestamp int64            `json:"gameEndTimestamp"` // 試合終了時刻 (Unix milliseconds)
		GameMode         string           `json:"gameMode"`
		GameType         string           `json:"gameType"`
		QueueID          int              `json:"queueId"`
		Participants     []ParticipantDTO `json:"participants"`
	} `json:"info"`
}

// ParticipantDTO MatchDTO内の参加者情報 (必要な部分のみ抜粋)
type ParticipantDTO struct {
	PUUID        string `json:"puuid"`
	SummonerName string `json:"summonerName"`
	Win          bool   `json:"win"`
	TeamID       int    `json:"teamId"`
	// LP関連の情報はここにはない
}

var (
	discordToken  string
	riotAPIKey    string
	lolPlayersEnv []string // .envから読み込んだサモナーリスト
)

func init() {
	// ../.env を指定してプロジェクトルートの .env ファイルを読み込む
	err := godotenv.Load("../.env")
	if err != nil {
		log.Println("Error loading .env file, using environment variables")
	}

	discordToken = os.Getenv("DISCORD_TOKEN")
	if discordToken == "" {
		log.Fatal("DISCORD_TOKEN environment variable not set")
	}

	riotAPIKey = os.Getenv("RIOT_API_KEY")
	if riotAPIKey == "" {
		log.Fatal("RIOT_API_KEY environment variable not set")
	}

	lolPlayersStr := os.Getenv("LOL_PLAYERS")
	if lolPlayersStr != "" {
		lolPlayersEnv = strings.Split(lolPlayersStr, ",")
		log.Printf("Loaded %d players from LOL_PLAYERS env: %v", len(lolPlayersEnv), lolPlayersEnv)
	} else {
		log.Println("LOL_PLAYERS environment variable not set or empty.")
	}
}

func main() {
	dg, err := discordgo.New("Bot " + discordToken)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}

	dg.AddHandler(messageCreate)

	err = dg.Open()
	if err != nil {
		log.Fatalf("Error opening connection: %v", err)
	}

	fmt.Println("Bot is now running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	// HTTPサーバーをgoroutineで起動
	go startHttpServer()

	dg.Close()
}

// startHttpServer はKoyebのヘルスチェック用のHTTPサーバーを起動します。
func startHttpServer() {
	port := os.Getenv(httpPortEnvVar)
	if port == "" {
		port = defaultHttpPort
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Discord Bot is running.")
	})

	log.Printf("HTTP server listening on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Error starting HTTP server: %v", err)
	}
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}

	// "!ranking" コマンドを先に判定する
	if m.Content == "!ranking" {
		if len(lolPlayersEnv) == 0 {
			s.ChannelMessageSend(m.ChannelID, ".envにLOL_PLAYERSが設定されていません。")
			return
		}

		// まず「集計中」のメッセージを送信
		processingMsg, err := s.ChannelMessageSend(m.ChannelID, "ランキング集計中です。少々お待ちください... ⏳")
		if err != nil {
			log.Printf("Error sending processing message: %v", err)
			// エラーが発生しても処理は続行する
		}

		var playerRanks []PlayerRankInfo

		for _, rawSummonerName := range lolPlayersEnv {
			// APIレート制限を考慮して遅延を入れる
			time.Sleep(apiRequestDelay)

			parts := strings.Split(rawSummonerName, "#")
			if len(parts) != 2 {
				log.Printf("Invalid Riot ID format in LOL_PLAYERS: %s", rawSummonerName)
				// エラー情報は表示しないか、別途集約する
				continue
			}
			gameName := parts[0]
			tagLine := parts[1]

			account, err := getAccountByRiotID(gameName, tagLine)
			if err != nil {
				log.Printf("Error getting PUUID for %s (from LOL_PLAYERS): %v", rawSummonerName, err)
				continue
			}

			time.Sleep(apiRequestDelay)
			summoner, err := getSummonerByPUUID(account.PUUID)
			if err != nil {
				log.Printf("Error getting summoner info for %s (PUUID: %s, from LOL_PLAYERS): %v", rawSummonerName, account.PUUID, err)
				continue
			}

			time.Sleep(apiRequestDelay)
			leagueEntries, err := getLeagueEntriesBySummonerID(summoner.ID)
			if err != nil {
				log.Printf("Error getting league entries for %s (Summoner ID: %s, from LOL_PLAYERS): %v", rawSummonerName, summoner.ID, err)
				continue
			}

			foundRank := false
			for _, entry := range leagueEntries {
				if entry.QueueType == "RANKED_SOLO_5x5" {
					tierVal, rankVal := getRankValues(entry.Tier, entry.Rank)
					playerRanks = append(playerRanks, PlayerRankInfo{
						RiotID:       rawSummonerName,
						Tier:         entry.Tier,
						Rank:         entry.Rank,
						LeaguePoints: entry.LeaguePoints,
						TierValue:    tierVal,
						RankValue:    rankVal,
					})
					foundRank = true
					break
				}
			}
			if !foundRank {
				// ランク情報がない場合もリストに追加する（アンランクとして扱う）
				playerRanks = append(playerRanks, PlayerRankInfo{
					RiotID:       rawSummonerName,
					Tier:         "UNRANKED",
					Rank:         "",
					LeaguePoints: 0,
					TierValue:    -1, // UNRANKEDは最下位
					RankValue:    -1,
				})
			}
		}

		// ランクでソート (Tier DESC, Rank DESC, LP DESC)
		sort.SliceStable(playerRanks, func(i, j int) bool {
			if playerRanks[i].TierValue != playerRanks[j].TierValue {
				return playerRanks[i].TierValue > playerRanks[j].TierValue
			}
			if playerRanks[i].RankValue != playerRanks[j].RankValue {
				return playerRanks[i].RankValue > playerRanks[j].RankValue
			}
			return playerRanks[i].LeaguePoints > playerRanks[j].LeaguePoints
		})

		var rankedMessages []string
		rankedMessages = append(rankedMessages, "**LOLプレイヤーランキング** :trophy:") // タイトル追加

		for i, pr := range playerRanks {
			// RiotID (GameName#TagLine) から GameName と TagLine を再分割してOP.GGリンクを作成
			riotIDParts := strings.Split(pr.RiotID, "#")
			opggLink := ""
			if len(riotIDParts) == 2 {
				// OP.GGのURLエンコードはハイフン区切りなので、TagLineもそのまま結合
				opggName := url.PathEscape(riotIDParts[0])
				opggTag := url.PathEscape(riotIDParts[1])
				// OP.GGのURLでは、GameNameとTagLineの間にハイフンが入る場合と入らない場合がある。
				// 一般的には {GameName}-{TagLine} だが、一部の古いアカウントや特殊な名前では異なる場合も。
				// Riot IDの仕様に厳密に従うなら、Account APIから返されるgameNameとtagLineを使うべきだが、
				// ここでは入力されたRiotIDを基に生成する。
				// OP.GGの日本リージョンのURL形式に合わせる
				opggLink = fmt.Sprintf("https://www.op.gg/summoners/jp/%s-%s", opggName, opggTag)
			}

			if pr.Tier == "UNRANKED" {
				if opggLink != "" {
					// URLを <> で囲んでプレビューを抑制
					rankedMessages = append(rankedMessages, fmt.Sprintf("**%d位**： [`%s`](<%s>) (UNRANKED)", i+1, pr.RiotID, opggLink))
				} else {
					rankedMessages = append(rankedMessages, fmt.Sprintf("**%d位**： `%s` (UNRANKED)", i+1, pr.RiotID))
				}
			} else {
				if opggLink != "" {
					// URLを <> で囲んでプレビューを抑制
					rankedMessages = append(rankedMessages, fmt.Sprintf("**%d位**： [`%s`](<%s>) (**%s %s** %dLP)", i+1, pr.RiotID, opggLink, strings.Title(strings.ToLower(pr.Tier)), pr.Rank, pr.LeaguePoints))
				} else {
					rankedMessages = append(rankedMessages, fmt.Sprintf("**%d位**： `%s` (**%s %s** %dLP)", i+1, pr.RiotID, strings.Title(strings.ToLower(pr.Tier)), pr.Rank, pr.LeaguePoints))
				}
			}
		}

		if len(playerRanks) > 0 {
			finalMessage := strings.Join(rankedMessages, "\n")
			if processingMsg != nil {
				_, err = s.ChannelMessageEdit(m.ChannelID, processingMsg.ID, finalMessage)
				if err != nil {
					log.Printf("Error editing message: %v. Sending new message instead.", err)
					s.ChannelMessageSend(m.ChannelID, finalMessage) // 編集に失敗したら新しいメッセージとして送信
				}
			} else {
				s.ChannelMessageSend(m.ChannelID, finalMessage) // 初期のメッセージ送信に失敗していた場合
			}
		} else {
			finalMessage := "ランク情報を取得できるプレイヤーがいませんでした。"
			if processingMsg != nil {
				_, err = s.ChannelMessageEdit(m.ChannelID, processingMsg.ID, finalMessage)
				if err != nil {
					log.Printf("Error editing message: %v. Sending new message instead.", err)
					s.ChannelMessageSend(m.ChannelID, finalMessage)
				}
			} else {
				s.ChannelMessageSend(m.ChannelID, finalMessage)
			}
		}

	} else if strings.HasPrefix(m.Content, "!rank") { // "!ranking" の後に "!rank" を判定
		args := strings.Split(m.Content, " ")
		if len(args) < 2 {
			s.ChannelMessageSend(m.ChannelID, "使用方法: !rank <サモナー名1> [サモナー名2] ...")
			return
		}

		summonerNames := args[1:]
		var rankInfos []string

		for _, rawSummonerName := range summonerNames {
			// APIレート制限を考慮して遅延を入れる
			time.Sleep(apiRequestDelay)

			parts := strings.Split(rawSummonerName, "#")
			if len(parts) != 2 {
				log.Printf("Invalid Riot ID format: %s", rawSummonerName)
				rankInfos = append(rankInfos, fmt.Sprintf("%s: Riot IDの形式が正しくありません (例: GameName#TagLine)", rawSummonerName))
				continue
			}
			gameName := parts[0]
			tagLine := parts[1]

			account, err := getAccountByRiotID(gameName, tagLine)
			if err != nil {
				log.Printf("Error getting PUUID for %s: %v", rawSummonerName, err)
				rankInfos = append(rankInfos, fmt.Sprintf("%s: アカウント情報を取得できませんでした。", rawSummonerName))
				continue
			}

			// APIレート制限を考慮して遅延を入れる
			time.Sleep(apiRequestDelay)
			summoner, err := getSummonerByPUUID(account.PUUID)
			if err != nil {
				log.Printf("Error getting summoner info for %s (PUUID: %s): %v", rawSummonerName, account.PUUID, err)
				rankInfos = append(rankInfos, fmt.Sprintf("%s: サモナー情報を取得できませんでした。", rawSummonerName))
				continue
			}

			// APIレート制限を考慮して遅延を入れる
			time.Sleep(apiRequestDelay)
			leagueEntries, err := getLeagueEntriesBySummonerID(summoner.ID)
			if err != nil {
				log.Printf("Error getting league entries for %s (Summoner ID: %s): %v", rawSummonerName, summoner.ID, err)
				rankInfos = append(rankInfos, fmt.Sprintf("%s: ランク情報を取得できませんでした。", rawSummonerName))
				continue
			}

			var soloRankInfo string
			for _, entry := range leagueEntries {
				if entry.QueueType == "RANKED_SOLO_5x5" {
					// rawSummonerName を使用して、どのプレイヤーの情報か明確にする
					soloRankInfo = fmt.Sprintf("%s: %s %s %dLP (%dW/%dL)", rawSummonerName, entry.Tier, entry.Rank, entry.LeaguePoints, entry.Wins, entry.Losses)
					break
				}
			}
			if soloRankInfo == "" {
				soloRankInfo = fmt.Sprintf("%s: ソロランク情報なし", rawSummonerName)
			}
			rankInfos = append(rankInfos, soloRankInfo)
		}
		s.ChannelMessageSend(m.ChannelID, strings.Join(rankInfos, "\n"))
	} else if strings.HasPrefix(m.Content, "!add ") {
		addSummonerRiotID := strings.TrimSpace(strings.TrimPrefix(m.Content, "!add "))
		if addSummonerRiotID == "" {
			s.ChannelMessageSend(m.ChannelID, "追加するRiot IDを指定してください (例: !add GameName#TagLine)")
			return
		}
		parts := strings.Split(addSummonerRiotID, "#")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			s.ChannelMessageSend(m.ChannelID, "Riot IDの形式が正しくありません (例: GameName#TagLine)")
			return
		}

		// 既にリストに存在するか確認
		for _, existingPlayer := range lolPlayersEnv {
			if existingPlayer == addSummonerRiotID {
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("`%s` は既に追加されています。", addSummonerRiotID))
				return
			}
		}

		// Riot APIで実在確認 (任意だが推奨)
		// この部分は getAccountByRiotID を流用できる
		_, err := getAccountByRiotID(parts[0], parts[1])
		if err != nil {
			log.Printf("Error verifying Riot ID %s for !add command: %v", addSummonerRiotID, err)
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("`%s` のアカウント情報を確認できませんでした。Riot IDが正しいか確認してください。", addSummonerRiotID))
			return
		}

		err = addPlayerToEnvFile(addSummonerRiotID)
		if err != nil {
			log.Printf("Error adding player %s to .env file: %v", addSummonerRiotID, err)
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("`%s` の追加中にエラーが発生しました。", addSummonerRiotID))
			return
		}

		// メモリ上のリストも更新
		lolPlayersEnv = append(lolPlayersEnv, addSummonerRiotID)
		log.Printf("Added %s to LOL_PLAYERS. New list: %v", addSummonerRiotID, lolPlayersEnv)
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("`%s` をランキングリストに追加しました。", addSummonerRiotID))

	} else if strings.HasPrefix(m.Content, "!daystats ") {
		handleDayStatsCommand(s, m)
	} else if m.Content == "!help" {
		helpMessage := "コマンド一覧:\n" +
			"```\n" +
			"!ranking                       : 登録プレイヤーのランキングを表示します。\n" +
			"!rank <RiotID> [RiotID2...]   : 指定したプレイヤーの現在のランク情報を表示します。\n" +
			"                                 RiotIDは GameName#TagLine の形式です。\n" +
			"!add <RiotID>                  : ランキング対象にプレイヤーを追加します。\n" +
			"                                 RiotIDは GameName#TagLine の形式です。\n" +
			"!daystats <RiotID> [日付]      : 指定したプレイヤーの特定日の戦績(AM5時～翌AM5時)を表示します。\n" +
			"                                 RiotIDは GameName#TagLine の形式です。\n" +
			"                                 日付は YYYYMMDD 形式で指定します (例: 20231027)。\n" +
			"                                 日付を省略した場合は実行日の戦績を表示します。\n" +
			"```"
		s.ChannelMessageSend(m.ChannelID, helpMessage)
	}
}

func handleDayStatsCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	args := strings.Split(m.Content, " ")
	if len(args) < 2 {
		s.ChannelMessageSend(m.ChannelID, "使用方法: !daystats <RiotID (GameName#TagLine)> [日付 YYYYMMDD]")
		return
	}
	riotID := args[1]
	dateStr := ""
	if len(args) > 2 {
		dateStr = args[2]
	}

	parts := strings.Split(riotID, "#")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("`%s` のRiot IDの形式が正しくありません (例: GameName#TagLine)", riotID))
		return
	}
	gameName := parts[0]
	tagLine := parts[1]

	jst := time.FixedZone("Asia/Tokyo", 9*60*60)
	var targetDate time.Time
	var dateParseError error

	if dateStr == "" { // 日付指定なし
		now := time.Now().In(jst)
		if now.Hour() < 5 {
			targetDate = now.AddDate(0, 0, -1) // 昨日の日付を基準
		} else {
			targetDate = now // 今日の日付を基準
		}
	} else { // 日付指定あり
		targetDate, dateParseError = time.ParseInLocation("20060102", dateStr, jst)
		if dateParseError != nil {
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("日付の形式が正しくありません。YYYYMMDD形式で指定してください (例: 20231027)。エラー: %v", dateParseError))
			return
		}
	}

	// 指定された日付のAM5:00から翌日のAM5:00までの範囲を計算
	startTime := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 5, 0, 0, 0, jst)
	endTime := startTime.AddDate(0, 0, 1) // 翌日のAM5:00

	startTimeUnix := startTime.Unix()
	endTimeUnix := endTime.Unix() // この時刻は含まない (exclusive)

	processingMsgText := fmt.Sprintf("`%s` の %s AM5:00 ～ %s AM5:00 の戦績を集計中です... ⏳",
		riotID, startTime.Format("2006/01/02"), endTime.Format("2006/01/02"))
	processingMsg, err := s.ChannelMessageSend(m.ChannelID, processingMsgText)
	if err != nil {
		log.Printf("Error sending processing message for !today: %v", err)
	}

	account, err := getAccountByRiotID(gameName, tagLine)
	if err != nil {
		errMsg := fmt.Sprintf("`%s` のアカウント情報を取得できませんでした: %v", riotID, err)
		log.Println(errMsg)
		if processingMsg != nil {
			s.ChannelMessageEdit(m.ChannelID, processingMsg.ID, errMsg)
		} else {
			s.ChannelMessageSend(m.ChannelID, errMsg)
		}
		return
	}

	// ランク戦(RANKED_SOLO_5x5)のMatch IDリストを取得
	matchIDs, err := getMatchIDsByPUUIDInTimeRange(account.PUUID, startTimeUnix, endTimeUnix, rankedSoloQueueID, 100, riotAPIKey)
	if err != nil {
		errMsg := fmt.Sprintf("`%s` の試合履歴を取得できませんでした (%s AM5:00 - %s AM5:00): %v",
			riotID, startTime.Format("2006/01/02"), endTime.Format("2006/01/02"), err)
		log.Println(errMsg)
		if processingMsg != nil {
			s.ChannelMessageEdit(m.ChannelID, processingMsg.ID, errMsg)
		} else {
			s.ChannelMessageSend(m.ChannelID, errMsg)
		}
		return
	}

	if len(matchIDs) == 0 {
		msg := fmt.Sprintf("`%s` は %s AM5:00 ～ %s AM5:00 の間にランク戦(ソロ/デュオ)をプレイしていません。",
			riotID, startTime.Format("2006/01/02"), endTime.Format("2006/01/02"))
		if processingMsg != nil {
			s.ChannelMessageEdit(m.ChannelID, processingMsg.ID, msg)
		} else {
			s.ChannelMessageSend(m.ChannelID, msg)
		}
		return
	}

	var wins, losses int
	for _, matchID := range matchIDs {
		time.Sleep(apiRequestDelay) // APIレート制限
		matchDetails, err := getMatchDetails(matchID, riotAPIKey)
		if err != nil {
			log.Printf("Error getting match details for %s (matchID: %s): %v", riotID, matchID, err)
			continue
		}

		// gameCreationはミリ秒なので /1000 してUnix秒に変換
		matchTimeUnix := matchDetails.Info.GameCreation / 1000
		// 試合が指定範囲内かつQueueIDが正しいか確認
		if matchTimeUnix >= startTimeUnix && matchTimeUnix < endTimeUnix && matchDetails.Info.QueueID == rankedSoloQueueID {
			for _, p := range matchDetails.Info.Participants {
				if p.PUUID == account.PUUID {
					if p.Win {
						wins++
					} else {
						losses++
					}
					break
				}
			}
		}
	}

	resultMsg := fmt.Sprintf("`%s` の %s AM5:00 ～ %s AM5:00 のランク戦績 (ソロ/デュオ):\n**%d勝 %d敗**",
		riotID, startTime.Format("2006/01/02"), endTime.Format("2006/01/02"), wins, losses)

	if processingMsg != nil {
		_, err = s.ChannelMessageEdit(m.ChannelID, processingMsg.ID, resultMsg)
		if err != nil {
			log.Printf("Error editing !today result message: %v", err)
			s.ChannelMessageSend(m.ChannelID, resultMsg) // 編集失敗時は新規送信
		}
	} else {
		s.ChannelMessageSend(m.ChannelID, resultMsg)
	}
}

// updateEnvFile は .env ファイルの指定されたキーの値を更新または追加します。
// キーが存在しない場合は新しい行として追加します。
func updateEnvFile(key, value string) error {
	envFilePath := "../.env" // main.goからの相対パス
	input, err := os.ReadFile(envFilePath)
	if err != nil {
		// .envファイルが存在しない場合は新規作成を試みる
		if os.IsNotExist(err) {
			log.Printf(".env file not found at %s, creating a new one.", envFilePath)
			content := fmt.Sprintf("%s=%s\n", key, value)
			return os.WriteFile(envFilePath, []byte(content), 0644)
		}
		return err
	}

	lines := strings.Split(string(input), "\n")
	found := false
	for i, line := range lines {
		if strings.HasPrefix(line, key+"=") {
			lines[i] = fmt.Sprintf("%s=%s", key, value)
			found = true
			break
		}
	}

	if !found {
		// キーが見つからなければ末尾に追加 (最終行が空行でない場合を考慮)
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			// 最終行が空なら、その一つ手前（実質的な最終行）の次に追加
			if len(lines) > 1 {
				lines[len(lines)-1] = fmt.Sprintf("%s=%s", key, value)
				lines = append(lines, "") // 新しい最終空行
			} else { // ファイルが空行のみだった場合
				lines[0] = fmt.Sprintf("%s=%s", key, value)
				lines = append(lines, "")
			}
		} else {
			lines = append(lines, fmt.Sprintf("%s=%s", key, value))
		}
	}

	output := strings.Join(lines, "\n")
	// 末尾に不要な空行が複数できないように調整
	output = strings.TrimRight(output, "\n") + "\n"

	return os.WriteFile(envFilePath, []byte(output), 0644)
}

// addPlayerToEnvFile は LOL_PLAYERS に新しいプレイヤーを追加します。
func addPlayerToEnvFile(newPlayerRiotID string) error {
	envFilePath := "../.env"
	input, err := os.ReadFile(envFilePath)
	if err != nil {
		// .envファイルが存在しない場合は、新規作成と同様の処理を行う
		if os.IsNotExist(err) {
			log.Printf(".env file not found at %s, creating LOL_PLAYERS entry.", envFilePath)
			return updateEnvFile("LOL_PLAYERS", newPlayerRiotID)
		}
		return fmt.Errorf("failed to read .env file: %w", err)
	}

	lines := strings.Split(string(input), "\n")
	var currentPlayers []string
	foundKey := false
	key := "LOL_PLAYERS"

	for _, line := range lines {
		if strings.HasPrefix(line, key+"=") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 && parts[1] != "" {
				currentPlayers = strings.Split(parts[1], ",")
			}
			foundKey = true
			break // LOL_PLAYERS が見つかったらループを抜ける
		}
	}

	// 重複チェック
	for _, p := range currentPlayers {
		if p == newPlayerRiotID {
			return nil // 既に追加されていれば何もしない
		}
	}
	currentPlayers = append(currentPlayers, newPlayerRiotID)

	// currentPlayersから空の要素を削除（Splitで空文字列が生まれる場合があるため）
	var cleanedPlayers []string
	for _, p := range currentPlayers {
		if p != "" {
			cleanedPlayers = append(cleanedPlayers, p)
		}
	}

	if !foundKey {
		// LOL_PLAYERS キー自体が .env にない場合 (updateEnvFileが対応するが、明示的に)
		log.Printf("LOL_PLAYERS key not found in .env, adding new entry.")
	}

	return updateEnvFile(key, strings.Join(cleanedPlayers, ","))
}

func getRankValues(tier, rank string) (int, int) {
	tierMap := map[string]int{
		"CHALLENGER":  10,
		"GRANDMASTER": 9,
		"MASTER":      8,
		"DIAMOND":     7,
		"EMERALD":     6,
		"PLATINUM":    5,
		"GOLD":        4,
		"SILVER":      3,
		"BRONZE":      2,
		"IRON":        1,
		"UNRANKED":    0,
	}
	rankMap := map[string]int{
		"I":   4,
		"II":  3,
		"III": 2,
		"IV":  1,
		"":    0, // UNRANKEDの場合など
	}
	return tierMap[strings.ToUpper(tier)], rankMap[strings.ToUpper(rank)]
}

// getMatchIDsByPUUIDInTimeRange は指定されたPUUIDと時間範囲内の特定のキュータイプの試合IDリストを取得します。
// startTimeUnix, endTimeUnix はUnixタイムスタンプ(秒)。endTimeUnixはexclusive。
// queueID: 420 (RANKED_SOLO_5x5), 440 (RANKED_FLEX_SR)など。
// count: 取得する試合数 (1-100)。
func getMatchIDsByPUUIDInTimeRange(puuid string, startTimeUnix int64, endTimeUnix int64, queueID int, count int, apiKey string) ([]string, error) {
	// Riot APIのMatch-V5では、startTime, endTime, queue, type, start, count のパラメータが利用可能
	apiURL := fmt.Sprintf("%s/lol/match/v5/matches/by-puuid/%s/ids?startTime=%d&endTime=%d&queue=%d&type=ranked&count=%d",
		riotMatchAPIBaseURL, puuid, startTimeUnix, endTimeUnix, queueID, count)
	log.Printf("Requesting Match IDs API URL: %s", apiURL)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for match IDs: %w", err)
	}
	req.Header.Set("X-Riot-Token", apiKey)

	client := &http.Client{Timeout: 10 * time.Second} // タイムアウト設定
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request for match IDs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Riot Match IDs API returned status %d for PUUID %s. Response: %s", resp.StatusCode, puuid, string(bodyBytes))
	}

	var matchIDs []string
	if err := json.NewDecoder(resp.Body).Decode(&matchIDs); err != nil {
		return nil, fmt.Errorf("failed to decode match IDs response: %w", err)
	}
	return matchIDs, nil
}

// getMatchDetails は指定されたMatch IDの試合詳細を取得します。
func getMatchDetails(matchID string, apiKey string) (*MatchDTO, error) {
	apiURL := fmt.Sprintf("%s/lol/match/v5/matches/%s", riotMatchAPIBaseURL, matchID)
	log.Printf("Requesting Match Detail API URL: %s", apiURL)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for match details: %w", err)
	}
	req.Header.Set("X-Riot-Token", apiKey)

	client := &http.Client{Timeout: 10 * time.Second} // タイムアウト設定
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request for match details: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Riot Match Detail API returned status %d for MatchID %s. Response: %s", resp.StatusCode, matchID, string(bodyBytes))
	}

	var matchDetails MatchDTO
	if err := json.NewDecoder(resp.Body).Decode(&matchDetails); err != nil {
		return nil, fmt.Errorf("failed to decode match details response: %w", err)
	}
	return &matchDetails, nil
}

func getAccountByRiotID(gameName, tagLine string) (*AccountDTO, error) {
	escapedGameName := url.PathEscape(gameName)
	escapedTagLine := url.PathEscape(tagLine)
	apiURL := fmt.Sprintf("%s/riot/account/v1/accounts/by-riot-id/%s/%s", riotAccountAPIBaseURL, escapedGameName, escapedTagLine)
	log.Printf("Requesting Account API URL: %s", apiURL) // リクエストURLをログ出力
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Riot-Token", riotAPIKey)

	client := &http.Client{Timeout: 10 * time.Second} // タイムアウト設定
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			log.Printf("Error reading response body: %v", readErr)
			return nil, fmt.Errorf("Riot Account API returned status code: %d for Riot ID %s#%s and failed to read response body", resp.StatusCode, gameName, tagLine)
		}
		return nil, fmt.Errorf("Riot Account API returned status code: %d for Riot ID %s#%s. Response: %s", resp.StatusCode, gameName, tagLine, string(bodyBytes))
	}

	var account AccountDTO
	if err := json.NewDecoder(resp.Body).Decode(&account); err != nil {
		return nil, err
	}
	return &account, nil
}

func getSummonerByPUUID(puuid string) (*SummonerDTO, error) {
	apiURL := fmt.Sprintf("%s/lol/summoner/v4/summoners/by-puuid/%s", riotLolAPIBaseURL, puuid)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Riot-Token", riotAPIKey)

	client := &http.Client{Timeout: 10 * time.Second} // タイムアウト設定
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Riot LoL API (Summoner by PUUID) returned status code: %d for PUUID %s", resp.StatusCode, puuid)
	}

	var summoner SummonerDTO
	if err := json.NewDecoder(resp.Body).Decode(&summoner); err != nil {
		return nil, err
	}
	return &summoner, nil
}

func getLeagueEntriesBySummonerID(summonerID string) ([]LeagueEntryDTO, error) {
	apiURL := fmt.Sprintf("%s/lol/league/v4/entries/by-summoner/%s", riotLolAPIBaseURL, summonerID)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Riot-Token", riotAPIKey)

	client := &http.Client{Timeout: 10 * time.Second} // タイムアウト設定
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Riot API returned status code: %d", resp.StatusCode)
	}

	var leagueEntries []LeagueEntryDTO
	if err := json.NewDecoder(resp.Body).Decode(&leagueEntries); err != nil {
		return nil, err
	}
	return leagueEntries, nil
}
