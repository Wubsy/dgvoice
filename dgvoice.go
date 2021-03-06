/*******************************************************************************
 * This is very experimental code and probably a long way from perfect or
 * ideal.  Please provide feed back on areas that would improve performance
 *
 */

// Package dgvoice provides opus encoding and audio file playback for the
// Discordgo package.
package dgvoice

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"

	"github.com/bwmarrin/discordgo"
	"layeh.com/gopus"
)

// NOTE: This API is not final and these are likely to change.

// Technically the below settings can be adjusted however that poses
// a lot of other problems that are not handled well at this time.
// These below values seem to provide the best overall performance
const (
	channels  int = 2                   // 1 for mono, 2 for stereo
	frameRate int = 48000               // audio sampling rate
	frameSize int = 960                 // uint16 size of each audio frame
	maxBytes  int = (frameSize * 2) * 2 // max size of opus data
)

var (
	Volume = 75
	speakers    map[uint32]*gopus.Decoder
	opusEncoder *gopus.Encoder
	Run         *exec.Cmd
	sendpcm     bool
	recvpcm     bool
	recv        chan *discordgo.Packet
	send        chan []int16
	mu          sync.Mutex
	IsSpeaking = false
	ListReady = true
	Paused = false
)

// SendPCM will receive on the provied channel encode
// received PCM data into Opus then send that to Discordgo
func SendPCM(v *discordgo.VoiceConnection, pcm <-chan []int16) {

	// make sure this only runs one instance at a time.
	mu.Lock()
	if sendpcm || pcm == nil {
		mu.Unlock()
		return
	}
	sendpcm = true
	mu.Unlock()

	defer func() { sendpcm = false }()

	var err error

	opusEncoder, err = gopus.NewEncoder(frameRate, channels, gopus.Audio)

	if err != nil {
		fmt.Println("NewEncoder Error:", err)
		return
	}

	for {

		// read pcm from chan, exit if channel is closed.


		recv, ok := <-pcm


		if !ok {
			fmt.Println("PCM Channel closed.")
			return
		}

		// try encoding pcm frame with Opus
		opus, err := opusEncoder.Encode(recv, frameSize, maxBytes)
		if err != nil {
			fmt.Println("Encoding Error:", err)
			return
		}

		if v.Ready == false || v.OpusSend == nil {
			fmt.Printf("Discordgo not ready for opus packets. %+v : %+v", v.Ready, v.OpusSend)
			KillPlayer()
			ListReady = false
			IsSpeaking = false
			return
		}
		// send encoded opus data to the sendOpus channel
		v.OpusSend <- opus
	}
}

// ReceivePCM will receive on the the Discordgo OpusRecv channel and decode
// the opus audio into PCM then send it on the provided channel.
func ReceivePCM(v *discordgo.VoiceConnection, c chan *discordgo.Packet) {

	// make sure this only runs one instance at a time.
	mu.Lock()
	if recvpcm || c == nil {
		mu.Unlock()
		return
	}
	recvpcm = true
	mu.Unlock()

	defer func() { sendpcm = false }()
	var err error

	for {

		if v.Ready == false || v.OpusRecv == nil {
			fmt.Printf("Discordgo not ready to receive opus packets. %+v : %+v", v.Ready, v.OpusRecv)
			return
		}


		p, ok := <-v.OpusRecv
		if !ok {
			return
		}

		if speakers == nil {
			speakers = make(map[uint32]*gopus.Decoder)
		}

		_, ok = speakers[p.SSRC]
		if !ok {
			speakers[p.SSRC], err = gopus.NewDecoder(48000, 2)
			if err != nil {
				fmt.Println("error creating opus decoder:", err)
				continue
			}
		}

		p.PCM, err = speakers[p.SSRC].Decode(p.Opus, 960, false)
		if err != nil {
			fmt.Println("Error decoding opus data: ", err)
			continue
		}

		c <- p
	}
}

// PlayAudioFile will play the given filename to the already connected
// Discord voice server/channel.  voice websocket and udp socket
// must already be setup before this will work.
func PlayAudioFile(v *discordgo.VoiceConnection, filename string, s *discordgo.Session) (err error) {

	// Create a shell command "object" to run.

	if !IsSpeaking {
		Run = exec.Command("ffmpeg", "-i", filename, "-vol", strconv.Itoa(Volume), "-f", "s16le", "-ar", strconv.Itoa(frameRate), "-ac", strconv.Itoa(channels), "pipe:1")

		ffmpegout, err := Run.StdoutPipe()
		if err != nil {
			fmt.Println("StdoutPipe Error:", err)
			return err
		}

		ffmpegbuf := bufio.NewReaderSize(ffmpegout, 16384)

		// Starts the ffmpeg command
		err = Run.Start()
		if err != nil {
			fmt.Println("RunStart Error:", err)
			return err
		}

		// Send "speaking" packet over the voice websocket
		v.Speaking(true)
		IsSpeaking = true
		// Send not "speaking" packet over the websocket when we finish
		defer func() {
			v.Speaking(false)
			IsSpeaking = false
			s.UpdateStatus(1, "")
		}()

		// will actually only spawn one instance, a bit hacky.

		if send == nil {
			send = make(chan []int16, 2)

		}
		go SendPCM(v, send)
		for {

			v.RLock()
			if Paused {
				v.RUnlock()
				continue
			}
			v.RUnlock()

			// read data from ffmpeg stdout
			audiobuf := make([]int16, frameSize*channels)
			err = binary.Read(ffmpegbuf, binary.LittleEndian, &audiobuf)
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return err
			}
			if err != nil {
				fmt.Println("error reading from ffmpeg stdout :", err)
				return err
			}

			// Send received PCM to the sendPCM channel
			send <- audiobuf
		}
		IsSpeaking = false
	} else {
		fmt.Println("Already playing.")
	}

	return nil
}

func IsThisThingOn() bool { //taps mic
	if Run != nil {
		return true
	} else {
		return false
	}
}

// KillPlayer forces the player to stop by killing the ffmpeg cmd process
// this method may be removed later in favor of using chans or bools to
// request a stop.
func KillPlayer() {
	if Run != nil {
		Run.Process.Kill()
	}
}

type StreamingSession struct {
	sync.Mutex

	// If this channel is not nil, an error will be sen when finished (or nil if no error)
	done chan error

	//source OpusReader
	vc     *discordgo.VoiceConnection

	paused     bool
	framesSent int

	finished bool
	running  bool
	err      error // If an error occurred and we had to stop
}
