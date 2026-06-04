package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/asticode/go-astiav"
	"github.com/deepch/vdk/av"
	"github.com/deepch/vdk/codec/aacparser"
	"github.com/deepch/vdk/codec/opusparser"
)

// The WebRTC muxer (deepch/vdk format/webrtcv3) only forwards PCMA/PCMU/Opus
// audio and silently drops everything else — notably AAC, which UniFi/Protect
// cameras emit. audioTranscoder decodes AAC and re-encodes it to Opus so WebRTC
// clients get sound, while the MSE/HLS paths keep their native AAC untouched.
//
// One instance is created per WebRTC session and driven from that session's
// single packet-pump goroutine. It is NOT safe for concurrent use.

const (
	opusSampleRate = 48000
	opusBitrate    = 64000
)

// opusWebRTCCodecData advertises the transcoded track to the muxer as Opus.
// Always stereo/48k to match the browser's default `opus/48000/2` offer.
func opusWebRTCCodecData() av.CodecData {
	return opusparser.NewCodecData(2)
}

type audioTranscoder struct {
	idx int8 // stream index this transcoder owns (must match incoming pkt.Idx)

	decCtx *astiav.CodecContext
	encCtx *astiav.CodecContext
	swr    *astiav.SoftwareResampleContext
	fifo   *astiav.AudioFifo

	decPkt   *astiav.Packet
	decFrame *astiav.Frame
	resFrame *astiav.Frame
	encFrame *astiav.Frame
	encPkt   *astiav.Packet

	encFrameSize int
	pts          int64
	frameDur     time.Duration
}

// newAudioTranscoder builds an AAC->Opus transcoder for the audio stream at idx.
func newAudioTranscoder(codec av.AudioCodecData, idx int8) (_ *audioTranscoder, err error) {
	t := &audioTranscoder{idx: idx}
	// Tear down any partially-built state if we bail out part way through.
	defer func() {
		if err != nil {
			t.Close()
		}
	}()

	// ── Decoder (AAC) ──────────────────────────────────────────────────────
	aac, ok := codec.(aacparser.CodecData)
	if !ok {
		return nil, fmt.Errorf("audio transcoder: expected aacparser.CodecData, got %T", codec)
	}
	decCodec := astiav.FindDecoder(astiav.CodecIDAac)
	if decCodec == nil {
		return nil, errors.New("audio transcoder: AAC decoder not found")
	}
	if t.decCtx = astiav.AllocCodecContext(decCodec); t.decCtx == nil {
		return nil, errors.New("audio transcoder: alloc decoder context failed")
	}
	t.decCtx.SetSampleRate(codec.SampleRate())
	t.decCtx.SetChannelLayout(channelLayoutForCount(codec.ChannelLayout().Count()))
	// The AudioSpecificConfig carries sample rate / channel / object type.
	if err = t.decCtx.SetExtraData(aac.MPEG4AudioConfigBytes()); err != nil {
		return nil, fmt.Errorf("audio transcoder: set decoder extradata: %w", err)
	}
	if err = t.decCtx.Open(decCodec, nil); err != nil {
		return nil, fmt.Errorf("audio transcoder: open decoder: %w", err)
	}

	// ── Encoder (Opus, via libopus) ────────────────────────────────────────
	encCodec := astiav.FindEncoderByName("libopus")
	if encCodec == nil {
		return nil, errors.New("audio transcoder: libopus encoder not found (ffmpeg built without libopus?)")
	}
	if t.encCtx = astiav.AllocCodecContext(encCodec); t.encCtx == nil {
		return nil, errors.New("audio transcoder: alloc encoder context failed")
	}
	sampleFmt := astiav.SampleFormatS16
	if fmts := encCodec.SupportedSampleFormats(); len(fmts) > 0 {
		sampleFmt = fmts[0]
	}
	t.encCtx.SetSampleRate(opusSampleRate)
	t.encCtx.SetChannelLayout(astiav.ChannelLayoutStereo)
	t.encCtx.SetSampleFormat(sampleFmt)
	t.encCtx.SetBitRate(opusBitrate)
	t.encCtx.SetTimeBase(astiav.NewRational(1, opusSampleRate))
	if err = t.encCtx.Open(encCodec, nil); err != nil {
		return nil, fmt.Errorf("audio transcoder: open encoder: %w", err)
	}
	t.encFrameSize = t.encCtx.FrameSize()
	if t.encFrameSize <= 0 {
		t.encFrameSize = 960 // 20ms @ 48kHz — libopus default
	}
	t.frameDur = time.Duration(t.encFrameSize) * time.Second / opusSampleRate

	// ── Resampler + FIFO (decoder output -> 48k / stereo / encoder format) ──
	t.swr = astiav.AllocSoftwareResampleContext()
	t.decPkt = astiav.AllocPacket()
	t.decFrame = astiav.AllocFrame()
	t.encPkt = astiav.AllocPacket()

	// resFrame is the resampler's output buffer; its allocated size caps how
	// many samples a single ConvertFrame yields — the rest drains on later calls.
	t.resFrame = astiav.AllocFrame()
	t.resFrame.SetChannelLayout(astiav.ChannelLayoutStereo)
	t.resFrame.SetSampleFormat(sampleFmt)
	t.resFrame.SetSampleRate(opusSampleRate)
	t.resFrame.SetNbSamples(t.encFrameSize * 4)
	if err = t.resFrame.AllocBuffer(0); err != nil {
		return nil, fmt.Errorf("audio transcoder: alloc resample buffer: %w", err)
	}

	// encFrame holds exactly one Opus frame's worth of samples, read from the FIFO.
	t.encFrame = astiav.AllocFrame()
	t.encFrame.SetChannelLayout(astiav.ChannelLayoutStereo)
	t.encFrame.SetSampleFormat(sampleFmt)
	t.encFrame.SetSampleRate(opusSampleRate)
	t.encFrame.SetNbSamples(t.encFrameSize)
	if err = t.encFrame.AllocBuffer(0); err != nil {
		return nil, fmt.Errorf("audio transcoder: alloc encode buffer: %w", err)
	}

	t.fifo = astiav.AllocAudioFifo(sampleFmt, astiav.ChannelLayoutStereo.Channels(), t.encFrameSize)

	return t, nil
}

func channelLayoutForCount(channels int) astiav.ChannelLayout {
	if channels <= 1 {
		return astiav.ChannelLayoutMono
	}
	return astiav.ChannelLayoutStereo
}

// transcode converts one inbound AAC packet into zero or more Opus av.Packets,
// each ready to hand straight to the WebRTC muxer.
func (t *audioTranscoder) transcode(pkt av.Packet) ([]av.Packet, error) {
	if len(pkt.Data) == 0 {
		return nil, nil
	}

	if err := t.decPkt.FromData(pkt.Data); err != nil {
		return nil, err
	}
	defer t.decPkt.Unref()

	if err := t.decCtx.SendPacket(t.decPkt); err != nil {
		return nil, err
	}

	var out []av.Packet
	for {
		if err := t.decCtx.ReceiveFrame(t.decFrame); err != nil {
			if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
				break
			}
			return out, err
		}

		err := t.resampleAndEncode(t.decFrame, &out)
		t.decFrame.Unref()
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// resampleAndEncode pushes one decoded frame through the resampler and FIFO,
// emitting Opus packets for every complete encoder frame that becomes available.
func (t *audioTranscoder) resampleAndEncode(in *astiav.Frame, out *[]av.Packet) error {
	if err := t.swr.ConvertFrame(in, t.resFrame); err != nil {
		return err
	}
	for {
		if t.resFrame.NbSamples() > 0 {
			if _, err := t.fifo.Write(t.resFrame); err != nil {
				return err
			}
		}
		if err := t.encodeFromFifo(out, false); err != nil {
			return err
		}
		// Drain any samples the resampler is still holding to keep latency low.
		if t.swr.Delay(int64(opusSampleRate)) < int64(t.encFrameSize) {
			return nil
		}
		if err := t.swr.ConvertFrame(nil, t.resFrame); err != nil {
			return err
		}
		if t.resFrame.NbSamples() == 0 {
			return nil
		}
	}
}

// encodeFromFifo reads whole encoder frames out of the FIFO and encodes them.
func (t *audioTranscoder) encodeFromFifo(out *[]av.Packet, flush bool) error {
	for (flush && t.fifo.Size() > 0) || t.fifo.Size() >= t.encFrameSize {
		n, err := t.fifo.Read(t.encFrame)
		if err != nil {
			return err
		}
		t.encFrame.SetNbSamples(n)
		t.encFrame.SetPts(t.pts)
		t.pts += int64(n)

		if err := t.encCtx.SendFrame(t.encFrame); err != nil {
			return err
		}
		if err := t.receiveOpus(out); err != nil {
			return err
		}
	}
	return nil
}

func (t *audioTranscoder) receiveOpus(out *[]av.Packet) error {
	for {
		if err := t.encCtx.ReceivePacket(t.encPkt); err != nil {
			if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
				return nil
			}
			return err
		}
		// Copy: the packet's buffer is reused on the next ReceivePacket.
		data := append([]byte(nil), t.encPkt.Data()...)
		t.encPkt.Unref()
		*out = append(*out, av.Packet{
			Idx:      t.idx,
			Data:     data,
			Duration: t.frameDur,
		})
	}
}

func (t *audioTranscoder) Close() {
	if t.fifo != nil {
		t.fifo.Free()
	}
	if t.swr != nil {
		t.swr.Free()
	}
	for _, f := range []*astiav.Frame{t.decFrame, t.resFrame, t.encFrame} {
		if f != nil {
			f.Free()
		}
	}
	for _, p := range []*astiav.Packet{t.decPkt, t.encPkt} {
		if p != nil {
			p.Free()
		}
	}
	if t.decCtx != nil {
		t.decCtx.Free()
	}
	if t.encCtx != nil {
		t.encCtx.Free()
	}
}
