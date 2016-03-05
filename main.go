package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os/exec"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/b1naryth1ef/discordgo"
	"github.com/layeh/gopus"
	redis "gopkg.in/redis.v3"
)

var discord *discordgo.Session
var rcli *redis.Client

type Play struct {
	GuildID   string
	ChannelID string
	Sound     *Sound
}

var queues map[string]chan *Play = make(map[string]chan *Play)

type Sound struct {
	Name   string
	Weight int

	encodeChan chan []int16
	buffer     [][]byte
}

func createSound(Name string, Weight int) *Sound {
	return &Sound{
		Name:       Name,
		Weight:     Weight,
		encodeChan: make(chan []int16, 10),
		buffer:     make([][]byte, 0),
	}
}

var SOUNDS []*Sound = []*Sound{
	createSound("airhorn1", 1000),
	createSound("airhorn_reverb", 800),
	createSound("airhorn_spam", 800),
	createSound("airhorn_tripletap", 800),
	createSound("airhorn_fourtap", 800),
	createSound("airhorn_distant", 500),
	createSound("airhorn_echo", 500),
	createSound("airhorn_highfartlong", 200),
	createSound("airhorn_highfartshort", 200),
	createSound("airhorn_midshort", 100),
	createSound("airhorn_truck", 10),
}

var (
	SOUND_RANGE    = 5701
	BITRATE        = 128
	MAX_QUEUE_SIZE = 6
)

func (s *Sound) Encode() {
	encoder, err := gopus.NewEncoder(48000, 2, gopus.Audio)
	if err != nil {
		fmt.Println("NewEncoder Error:", err)
		return
	}

	encoder.SetBitrate(BITRATE * 1000)
	encoder.SetApplication(gopus.Audio)

	for {
		pcm, ok := <-s.encodeChan
		if !ok {
			// if chan closed, exit
			return
		}

		// try encoding pcm frame with Opus
		opus, err := encoder.Encode(pcm, 960, 960*2*2)
		if err != nil {
			fmt.Println("Encoding Error:", err)
			return
		}

		s.buffer = append(s.buffer, opus)
	}

}

func (s *Sound) Load() {
	s.encodeChan = make(chan []int16, 10)
	defer close(s.encodeChan)
	go s.Encode()

	ffmpeg := exec.Command("ffmpeg", "-i", "audio/"+s.Name+".wav", "-vol", "256", "-f", "s16le", "-ar", "48000", "-ac", "2", "pipe:1")

	stdout, err := ffmpeg.StdoutPipe()
	if err != nil {
		fmt.Println("StdoutPipe Error:", err)
		return
	}

	err = ffmpeg.Start()
	if err != nil {
		fmt.Println("RunStart Error:", err)
		return
	}

	for {
		// read data from ffmpeg stdout
		InBuf := make([]int16, 960*2)
		err = binary.Read(stdout, binary.LittleEndian, &InBuf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return
		}
		if err != nil {
			fmt.Println("error reading from ffmpeg stdout :", err)
			return
		}

		// write pcm data to the encodeChan
		s.encodeChan <- InBuf
	}
}

func (s *Sound) Play(vc *discordgo.VoiceConnection) {
	vc.Speaking(true)
	defer vc.Speaking(false)

	for _, buff := range s.buffer {
		vc.OpusSend <- buff
	}
}

func getCurrentVoiceChannel(user *discordgo.User, guild *discordgo.Guild) *discordgo.Channel {
	for _, vs := range guild.VoiceStates {
		if vs.UserID == user.ID {
			channel, _ := discord.Channel(vs.ChannelID)
			return channel
		}
	}
	return nil
}

func randomRange(min, max int) int {
	rand.Seed(time.Now().Unix())
	return rand.Intn(max-min) + min
}

func getRandomSound() *Sound {
	var i int
	number := randomRange(0, SOUND_RANGE)

	for _, item := range SOUNDS {
		i += item.Weight

		if number < i {
			return item
		}
	}

	return nil
}

func enqueuePlay(user *discordgo.User, guild *discordgo.Guild, sound *Sound) {
	// Grab the users voice channel
	// TODO: sometimes this isn't accurate
	channel := getCurrentVoiceChannel(user, guild)
	if channel == nil {
		log.WithFields(log.Fields{
			"user":  user.ID,
			"guild": guild.ID,
		}).Warning("Failed to find channel to play sound in")
		return
	}

	if sound == nil {
		sound = getRandomSound()
	}

	play := &Play{
		GuildID:   guild.ID,
		ChannelID: channel.ID,
		Sound:     sound,
	}

	// Check if we already have a connection to this guild
	_, exists := queues[guild.ID]

	if exists {
		if len(queues[guild.ID]) < MAX_QUEUE_SIZE {
			queues[guild.ID] <- play
		}
	} else {
		queues[guild.ID] = make(chan *Play, MAX_QUEUE_SIZE)
		playSound(play, nil)
	}

}

func playSound(play *Play, vc *discordgo.VoiceConnection) {
	var err error

	delay := 0

	log.WithFields(log.Fields{
		"channel": play.ChannelID,
		"sound":   play.Sound.Name,
		"delay":   delay,
	}).Info("Playing sound")

	if vc == nil {
		// Only calculate delay if its the first time joining the channel
		if randomRange(1, 10) == 5 {
			delay = randomRange(1000, 5000)
		}

		vc, err = discord.ChannelVoiceJoin(play.GuildID, play.ChannelID, false, false)
		vc.Receive = false
		if err != nil {
			log.WithFields(log.Fields{
				"error": err,
			}).Error("Failed to play sound")
		}

		vc.WaitUntilConnected()
	}

	if vc.ChannelID != play.ChannelID {
		vc.ChangeChannel(play.ChannelID)
		time.Sleep(time.Millisecond * 200)
	}

	time.Sleep(time.Millisecond * time.Duration(delay))
	_, err = rcli.Pipelined(func(pipe *redis.Pipeline) error {
		pipe.Incr("airhorn:total")
		pipe.Incr(fmt.Sprintf("airhorn:sound:%s:total", play.Sound.Name))
		pipe.Incr(fmt.Sprintf("airhorn:guild:%s:total", play.GuildID))
		pipe.Incr(fmt.Sprintf("airhorn:guild:%s:chan:%s:total", play.GuildID, play.ChannelID))
		return nil
	})

	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Warning("Failed to track stats in redis")
	}

	play.Sound.Play(vc)

	if len(queues[play.GuildID]) > 0 {
		play := <-queues[play.GuildID]
		playSound(play, vc)
	}

	time.Sleep(time.Millisecond * 350)
	delete(queues, play.GuildID)
	vc.Close()
}

func onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	content := strings.ToLower(m.Content)
	var sound *Sound

	if strings.HasPrefix(content, "!airhorn") {
		channel, _ := discord.Channel(m.ChannelID)
		guild, _ := discord.Guild(channel.GuildID)

		if channel == nil || guild == nil {
			fmt.Printf("ERROR could not grab channel/guild\n")
			return
		}

		if strings.Count(content, " ") == 1 {
			parts := strings.Split(content, " ")

			for _, s := range SOUNDS {
				if s.Name == parts[1] {
					sound = s
				}
			}
		}

		rcli.Incr("airhorn:cmd:total")
		go enqueuePlay(m.Author, guild, sound)
	}
}

func main() {
	var (
		Token = flag.String("t", "", "Discord Authentication Token")
		Redis = flag.String("r", "", "Redis Connection String")
		err   error
	)
	flag.Parse()

	log.Info("Preloading sounds...")
	for _, sound := range SOUNDS {
		sound.Load()
	}

	log.Info("Connecting to redis...")
	rcli = redis.NewClient(&redis.Options{Addr: *Redis, DB: 0})
	_, err = rcli.Ping().Result()

	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Fatal("Failed to connect to redis")
		return
	}

	log.Info("Starting discord session...")
	discord, err = discordgo.New(*Token)
	// discord.Debug = true
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Fatal("Failed to create discord session")
		return
	}

	discord.AddHandler(onMessageCreate)

	err = discord.Open()
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Fatal("Failed to create discord websocket connection")
		return
	}

	log.Info("AIRHORNBOT is ready to horn it up.")
	for {
		time.Sleep(time.Second * 1)
	}
}