package notify

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

// 托盘图标：尺寸必须是 32×32（Wails 会缩到系统小图标 16×16），
// 且两版【必须真的不同】—— 否则窗口隐藏时的「闪烁」看起来毫无变化，
// 这是最难被发现的静默失效：功能全在跑，用户却收不到任何视觉提示。
func TestTrayIcons(t *testing.T) {
	normal, alert := TrayIcon(), TrayAlertIcon()
	for name, data := range map[string][]byte{"tray.png": normal, "tray-alert.png": alert} {
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s 解码失败: %v", name, err)
		}
		if got := img.Bounds(); got.Dx() != 32 || got.Dy() != 32 {
			t.Errorf("%s 尺寸 %v，期望 32x32", name, got)
		}
	}

	a, err := png.Decode(bytes.NewReader(normal))
	if err != nil {
		t.Fatal(err)
	}
	b, err := png.Decode(bytes.NewReader(alert))
	if err != nil {
		t.Fatal(err)
	}
	diff := 0
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			ar, ag, ab, aa := a.At(x, y).RGBA()
			br, bg, bb, ba := b.At(x, y).RGBA()
			if ar != br || ag != bg || ab != bb || aa != ba {
				diff++
			}
		}
	}
	if diff < 20 {
		t.Fatalf("两版图标只有 %d 个像素不同，闪烁会看不出来", diff)
	}
}

// 提示音必须是合法的 RIFF/WAVE，时长在合理区间 —— 防的是「文件放错/生成截断，
// 表现为永远没声音」这种静默失效。
//
// 这里刻意只读文件头算时长，不解码音频：本包不引音频依赖，
// 而「头合法 + 字节数对得上」已经能挡住生成脚本写坏的情况。
func TestSoundAssetIsValidWav(t *testing.T) {
	data := Sound()
	if len(data) < 44 {
		t.Fatalf("提示音只有 %d 字节，连 WAV 头都不够", len(data))
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatal("不是 RIFF/WAVE 文件")
	}
	if string(data[12:16]) != "fmt " || string(data[36:40]) != "data" {
		t.Fatalf("WAV 结构异常：chunk 标识 = %q / %q", data[12:16], data[36:40])
	}

	channels := binary.LittleEndian.Uint16(data[22:24])
	byteRate := binary.LittleEndian.Uint32(data[28:32])
	bits := binary.LittleEndian.Uint16(data[34:36])
	dataSize := binary.LittleEndian.Uint32(data[40:44])

	if channels != 1 {
		t.Errorf("声道数 = %d，期望 1（提示音不需要立体声）", channels)
	}
	if bits != 16 {
		t.Errorf("位深 = %d，期望 16", bits)
	}
	if byteRate == 0 {
		t.Fatal("byteRate 为 0，音频参数不合法")
	}

	seconds := float64(dataSize) / float64(byteRate)
	if seconds < 0.3 || seconds > 1.0 {
		t.Fatalf("时长 %.2fs，期望 0.3~1.0s（太短听不清、太长会烦）", seconds)
	}

	// 声明的数据长度必须与实际字节数一致，否则播放到一半就断
	if int(dataSize)+44 != len(data) {
		t.Errorf("data 声明 %d 字节，实际文件 %d 字节（头 44）", dataSize, len(data))
	}
}

// 提示音不能是「一段静音」：那会让「有声音」这个需求静默失效，
// 而所有结构检查仍然通过。
func TestSoundAssetIsAudible(t *testing.T) {
	data := Sound()
	peak := 0
	for i := 44; i+1 < len(data); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(data[i : i+2])))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	// int16 满量程 32767；-6dBFS ≈ 16384。给足余量，只要求「明显不是静音」。
	if peak < 8000 {
		t.Fatalf("峰值只有 %d（满量程 32767），听起来会是静音", peak)
	}
	if peak > 32767 {
		t.Fatalf("峰值 %d 溢出 int16", peak)
	}
}
