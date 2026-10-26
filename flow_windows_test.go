//
// Copyright 2014-2026 Cristian Maglie. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

package serial

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestApplyFlowControl(t *testing.T) {
	rts := func(f uint32) uint32 { return f &^ dcbRTSControlDisableMask }
	var p windows.DCB
	p.Flags = dcbRTSControlEnable | dcbInX | dcbOutX | dcbOutXCTSFlow

	applyFlowControl(NoFlowControl, &p)
	if p.Flags&(dcbInX|dcbOutX|dcbOutXCTSFlow) != 0 || rts(p.Flags) != dcbRTSControlEnable {
		t.Errorf("none: flags %#x", p.Flags)
	}
	applyFlowControl(RTSCTSFlowControl, &p)
	if p.Flags&dcbOutXCTSFlow == 0 || rts(p.Flags) != dcbRTSControlHandshake || p.Flags&(dcbInX|dcbOutX) != 0 {
		t.Errorf("rts/cts: flags %#x", p.Flags)
	}
	applyFlowControl(XONXOFFFlowControl, &p)
	if p.Flags&(dcbInX|dcbOutX) != dcbInX|dcbOutX || p.Flags&dcbOutXCTSFlow != 0 || rts(p.Flags) != dcbRTSControlEnable {
		t.Errorf("xon/xoff: flags %#x (RTS must go back to program control)", p.Flags)
	}
}
