package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

func main() {
	token := os.Getenv("SLACK_TOKEN")
	if token == "" {
		panic("SLACK_TOKEN is not set")
	}
	ctx := context.Background()
	api := slack.New(token)

	team, err := api.GetTeamInfoContext(ctx)
	if err != nil {
		panic(err)
	}
	domain := fmt.Sprintf("%v.slack.com", team.Domain)
	fmt.Printf("%v\n", domain)

	domainDir := filepath.Join("archive", domain)
	err = os.MkdirAll(domainDir, 0777)
	if err != nil {
		panic(err)
	}

	users, err := api.GetUsersContext(ctx)
	if err != nil {
		panic(err)
	}
	userMap := make(map[string]string, len(users))
	for _, user := range users {
		userMap[user.ID] = user.RealName
	}
	channels, err := getAllChannels(ctx, api)
	if err != nil {
		panic(err)
	}
	now := time.Now().UTC()
	startTime := time.Date(
		now.Year(),
		now.Month(),
		1,
		0, 0, 0, 0,
		time.UTC,
	)
	for _, channel := range channels {
		if !channel.IsArchived && !channel.IsMember {
			fmt.Printf(
				"Not a member: ID: %v, Name: %v, IsArchived: %v, Created: %v\n",
				channel.ID,
				channel.Name,
				channel.IsArchived,
				channel.Created.Time(),
			)
			continue
		}
		channelDir := filepath.Join(domainDir, channel.Name)
		err = os.MkdirAll(channelDir, 0777)
		if err != nil {
			panic(err)
		}
		err := archiveChannel(ctx, api, userMap, &channel, channelDir, startTime)
		if err != nil {
			panic(err)
		}
	}
}

func timestampToTime(timestamp string) (time.Time, error) {
	idx := strings.IndexRune(timestamp, '.')
	if idx < 0 {
		sec, err := strconv.ParseInt(timestamp, 10, 64)
		if err != nil {
			return time.Time{}, err
		}
		return time.Unix(sec, 0), nil
	}
	timestampSec := timestamp[0:idx]
	timestampNano := "0." + timestamp[idx+1:]
	sec, err := strconv.ParseInt(timestampSec, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	secFloat, err := strconv.ParseFloat(timestampNano, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, int64(secFloat*1_000_0000_000)), nil
}

func getAllChannels(ctx context.Context, api *slack.Client) ([]slack.Channel, error) {
	var channels []slack.Channel
	var cursor string
	for {
		var gotChannels []slack.Channel
		var err error
		gotChannels, cursor, err = api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
			Types:  []string{"public_channel", "private_channel"},
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		channels = append(channels, gotChannels...)
		if cursor == "" {
			break
		}
	}
	sort.Slice(
		channels,
		func(i, j int) bool {
			return strings.Compare(channels[i].Name, channels[j].Name) < 0
		},
	)
	return channels, nil
}

type message struct {
	ChannelID string
	ThreadID  string
	Name      string
	Timestamp time.Time
	Text      string
}

func archiveChannel(
	ctx context.Context,
	api *slack.Client,
	userMap map[string]string,
	channel *slack.Channel,
	channelDir string,
	startTime time.Time,
) error {
	fmt.Printf(
		"ID: %v, Name: %v, IsArchived: %v, Created: %v\n",
		channel.ID,
		channel.Name,
		channel.IsArchived,
		channel.Created.Time(),
	)
	collected, err := isAlreadyCollected(channelDir, startTime.AddDate(0, -1, 0).Format("2006-01"))
	if err != nil {
		return err
	}
	if collected {
		fmt.Printf("    Already collected\n")
		return nil
	}
	var messages []message
	var cursor string
	currentYearMonth := ""
	for {
		param := &slack.GetConversationHistoryParameters{
			ChannelID: channel.ID,
			Cursor:    cursor,
			Inclusive: false,
		}
		if cursor == "" {
			param.Latest = strconv.FormatInt(startTime.Unix(), 10)
		}
		res, err := api.GetConversationHistoryContext(ctx, param)
		if err != nil {
			return err
		}
		cursor = res.ResponseMetaData.NextCursor
		for _, msg := range res.Messages {
			timestamp, err := timestampToTime(msg.Timestamp)
			if err != nil {
				return err
			}
			name, ok := userMap[msg.User]
			if !ok {
				name = msg.User
			}
			text := extractText(ctx, api, msg)
			yearMonth := timestamp.Format("2006-01") // yyyy-MM
			if currentYearMonth == "" {
				currentYearMonth = yearMonth
				file := filepath.Join(channelDir, fmt.Sprintf("%v.txt", currentYearMonth))
				if _, err := os.Stat(file); err == nil {
					// すでに収集済み
					return nil
				} else if !os.IsNotExist(err) {
					return err
				}
			} else if currentYearMonth != yearMonth {
				file := filepath.Join(channelDir, fmt.Sprintf("%v.txt", currentYearMonth))
				err := archiveMessages(ctx, api, userMap, file, messages)
				if err != nil {
					return err
				}
				currentYearMonth = yearMonth
				messages = nil
				file = filepath.Join(channelDir, fmt.Sprintf("%v.txt", currentYearMonth))
				if _, err := os.Stat(file); err == nil {
					// すでに収集済み
					return nil
				} else if !os.IsNotExist(err) {
					return err
				}
			}
			messages = append(messages, message{
				ChannelID: channel.ID,
				ThreadID:  msg.ThreadTimestamp,
				Name:      name,
				Timestamp: timestamp,
				Text:      text,
			})
		}
		if cursor == "" {
			break
		}
	}
	if len(messages) > 0 {
		file := filepath.Join(channelDir, fmt.Sprintf("%v.txt", currentYearMonth))
		err := archiveMessages(ctx, api, userMap, file, messages)
		if err != nil {
			return err
		}
	}
	return nil
}

func isAlreadyCollected(channelDir, yearMonth string) (bool, error) {
	file := filepath.Join(channelDir, fmt.Sprintf("%v.txt", yearMonth))
	if _, err := os.Stat(file); err == nil {
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	return false, nil
}

func extractText(ctx context.Context, api *slack.Client, msg slack.Message) string {
	var builder strings.Builder
	builder.WriteString(msg.Text)
	for _, attachement := range msg.Attachments {
		if attachement.Fallback == "" {
			continue
		}
		for _, line := range strings.Split(attachement.Fallback, "\n") {
			builder.WriteString("\n    ")
			builder.WriteString(line)
		}
	}
	for _, file := range msg.Files {
		fileInfo, _, _, err := api.GetFileInfoContext(ctx, file.ID, 0, 0)
		builder.WriteString("\n   添付ファイル: ")
		if err != nil {
			builder.WriteString("(エラー)")
			builder.WriteString(err.Error())
			fmt.Printf(
				"ERROR: Failed to get file info: %v in %v (%v)",
				file.ID,
				msg.Channel,
				msg.Text,
			)
		} else {
			builder.WriteString(fileInfo.Name)
		}
	}
	return builder.String()
}

func archiveMessages(ctx context.Context, api *slack.Client, userMap map[string]string, file string, messages []message) error {
	fh, err := os.Create(file)
	if err != nil {
		return err
	}
	defer fh.Close()
	writer := bufio.NewWriter(fh)
	defer writer.Flush()

	for idx := len(messages) - 1; idx >= 0; idx-- {
		msg := messages[idx]
		fmt.Fprintf(writer, "%v %v\n%v\n\n", msg.Name, msg.Timestamp.Format("2006-01-02 03:04:05"), msg.Text)
		err := writeThreadMessages(ctx, api, userMap, writer, msg)
		if err != nil {
			return err
		}
		writer.Flush()
	}
	fmt.Printf("    %v\n", file)
	return nil
}

func writeThreadMessages(ctx context.Context, api *slack.Client, userMap map[string]string, writer io.Writer, msg message) error {
	if msg.ThreadID == "" {
		return nil
	}
	var cursor string
	for {
		var threadMessages []slack.Message
		var err error
		threadMessages, _, cursor, err = api.GetConversationRepliesContext(
			ctx,
			&slack.GetConversationRepliesParameters{
				ChannelID: msg.ChannelID,
				Timestamp: msg.ThreadID,
				Cursor:    cursor,
			},
		)
		if err != nil {
			return err
		}
		for _, threadMessage := range threadMessages {
			if threadMessage.Timestamp == msg.ThreadID {
				continue
			}
			timestamp, err := timestampToTime(threadMessage.Timestamp)
			if err != nil {
				return err
			}
			name, ok := userMap[threadMessage.User]
			if !ok {
				name = threadMessage.User
			}
			text := extractText(ctx, api, threadMessage)
			fmt.Fprintf(
				writer,
				"    %v %v\n",
				name,
				timestamp.Format("2006-01-02 03:04:05"),
			)
			for _, line := range strings.Split(text, "\n") {
				fmt.Fprintf(writer, "    %v\n", line)
			}
			fmt.Fprintf(writer, "\n")
		}
		if cursor == "" {
			break
		}
	}
	return nil
}
