package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"google.golang.org/api/drive/v3"
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

	driveService, err := drive.NewService(ctx)
	if err != nil {
		panic(err)
	}

	folderID := os.Getenv("GOOGLE_DRIVE_FOLDER_ID")
	/*
		permissionList, err := driveService.Permissions.List(folderID).
			Fields("permissions(emailAddress)").
			Do()
		if err != nil {
			panic(err)
		}
		for _, permission := range permissionList.Permissions {
			fmt.Printf("%+v\n", *permission)
		}
	*/
	rootFolder, err := driveService.Files.Get(folderID).Fields("id, name").Do()
	if err != nil {
		panic(err)
	}
	if rootFolder.Name != domain {
		panic(fmt.Errorf("google drive folder name must be %v, but %v", domain, rootFolder.Name))
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
	now := time.Now()
	startTime := time.Date(
		now.Year(),
		now.Month(),
		1,
		0, 0, 0, 0,
		time.Local,
	)
	for _, channel := range channels {
		fmt.Printf(
			"ID: %v, Name: %v, IsArchived: %v, IsMember: %v, IsPrivate: %v\n",
			channel.ID,
			channel.Name,
			channel.IsArchived,
			channel.IsMember,
			channel.IsPrivate,
		)
		/*
			if channel.IsMember {
				_, err := api.LeaveConversationContext(ctx, channel.ID)
				if err != nil {
					panic(err)
				}
			}
		*/
		/*
			if !channel.IsArchived && !channel.IsMember {
				_, _, _, err := api.JoinConversationContext(ctx, channel.ID)
				if err != nil {
					fmt.Printf("    ERROR: Not a member/Failed to join: %+v\n", err)
					continue
				}
				fmt.Printf("    JOINED\n")
			}
		*/

		channelFolder, err := getOrCreateFolder(driveService, rootFolder, channel.Name)
		if err != nil {
			panic(err)
		}
		err = archiveChannel(ctx, api, driveService, userMap, &channel, channelFolder, startTime)
		if err != nil {
			panic(err)
		}
	}
}

func getOrCreateFolder(driveService *drive.Service, rootFolder *drive.File, name string) (*drive.File, error) {
	fileList, err := driveService.Files.List().
		Fields("files(id, name, mimeType, parents)").
		Q(fmt.Sprintf("'%s' in parents and name='%s'", rootFolder.Id, name)).
		Do()
	if err != nil {
		return nil, err
	}
	if len(fileList.Files) > 1 {
		return nil, fmt.Errorf("more than 1 folder found: %v", name)
	}
	if len(fileList.Files) == 1 {
		folder := fileList.Files[0]
		if folder.MimeType != "application/vnd.google-apps.folder" {
			return nil, fmt.Errorf("not a folder: %v", name)
		}
		return folder, nil
	}
	folderInfo := &drive.File{
		Name:     name,
		Parents:  []string{rootFolder.Id},
		MimeType: "application/vnd.google-apps.folder",
	}
	folder, err := driveService.Files.Create(folderInfo).
		Fields("id, name, mimeType, parents").
		Do()
	if err != nil {
		return nil, err
	}
	fmt.Printf("    Create: %v/%v (%v)\n", rootFolder.Name, folder.Name, folder.Id)
	return folder, nil
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
	driveService *drive.Service,
	userMap map[string]string,
	channel *slack.Channel,
	channelFolder *drive.File,
	startTime time.Time,
) error {
	collected, err := isAlreadyCollected(driveService, channelFolder, startTime.AddDate(0, -1, 0).Format("2006-01"))
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
				collected, err := isAlreadyCollected(
					driveService,
					channelFolder,
					currentYearMonth,
				)
				if err != nil {
					return err
				}
				if collected {
					// すでに収集済み
					return nil
				}
			} else if currentYearMonth != yearMonth {
				err := archiveMessages(ctx, api, driveService, userMap, channelFolder, currentYearMonth, messages)
				if err != nil {
					return err
				}
				currentYearMonth = yearMonth
				messages = nil
				collected, err := isAlreadyCollected(
					driveService,
					channelFolder,
					currentYearMonth,
				)
				if err != nil {
					return err
				}
				if collected {
					// すでに収集済み
					return nil
				}
			}
			var threadID string
			if msg.ThreadTimestamp != "" && msg.Timestamp == msg.ThreadTimestamp {
				threadID = msg.ThreadTimestamp
			}
			messages = append(messages, message{
				ChannelID: channel.ID,
				ThreadID:  threadID,
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
		err := archiveMessages(ctx, api, driveService, userMap, channelFolder, currentYearMonth, messages)
		if err != nil {
			return err
		}
	}
	return nil
}

func isAlreadyCollected(driveService *drive.Service, channelFolder *drive.File, yearMonth string) (bool, error) {
	name := fmt.Sprintf("%v.txt", yearMonth)
	fileList, err := driveService.Files.List().
		Fields("files(id, name, mimeType, parents)").
		Q(fmt.Sprintf("'%s' in parents and name='%s'", channelFolder.Id, name)).
		Do()
	if err != nil {
		return false, err
	}
	if len(fileList.Files) > 1 {
		return false, fmt.Errorf("more than 1 folder found: %v/%v", channelFolder.Name, name)
	}
	return len(fileList.Files) == 1, nil
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
				"ERROR: Failed to get file info: %v in %v (%v): %+v\n",
				file.ID,
				msg.Channel,
				msg.Text,
				err,
			)
		} else {
			builder.WriteString(fileInfo.Name)
		}
	}
	return builder.String()
}

func archiveMessages(
	ctx context.Context,
	api *slack.Client,
	driveService *drive.Service,
	userMap map[string]string,
	channelFolder *drive.File,
	yearMonth string,
	messages []message,
) error {
	writer := &bytes.Buffer{}
	for idx := len(messages) - 1; idx >= 0; idx-- {
		msg := messages[idx]
		fmt.Fprintf(writer, "%v %v\n%v\n\n", msg.Name, msg.Timestamp.Format("2006-01-02 03:04:05"), msg.Text)
		err := writeThreadMessages(ctx, api, userMap, writer, msg)
		if err != nil {
			return err
		}
	}

	fileInfo := &drive.File{
		Name:     fmt.Sprintf("%v.txt", yearMonth),
		Parents:  []string{channelFolder.Id},
		MimeType: "text/plain",
	}
	file, err := driveService.Files.Create(fileInfo).Media(writer).Do()
	if err != nil {
		return err
	}

	fmt.Printf("    %v (%v)\n", file.Name, file.Id)
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
