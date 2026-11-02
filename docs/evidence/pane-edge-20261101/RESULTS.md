# A white line at the edge of the pane

`harness/rig.mjs`: scratch daemon with `--state-dir` and the mock provider, real shell and renderer in the
pinned Electron. A black hosted video, muted, shown in the pane and captured with `screencapture -l`, the
window as the window server paints it. "Light pixels" are retina pixels at the pane's edge on a row or
column through the middle of the black page; the pane's frame is white, and 2 of them are one point.

## Cause

Below 320 points wide the pane gets a scaled viewport (320 wide, whole-pixel height), whose shape is never
exactly the pane's. `presentationFor` fits it inside and rounds down, so the page falls one point short.
Which side depends on how the height rounded: at the default pane width of 285, a 790 pane loses a point at
the bottom and a 789 one loses it at the right. The frame's white shows through. A page within a point of
its frame now fills it; a real difference of shape keeps its bars.

## Before, this branch without the fix

```
window h 900: pane 285x790, view 285x789, light pixels at right edge 0, at bottom edge 2
window h 899: pane 285x789, view 284x789, light pixels at right edge 2, at bottom edge 0
window h 898: pane 285x788, view 284x788, light pixels at right edge 2, at bottom edge 0
window h 897: pane 285x787, view 284x787, light pixels at right edge 2, at bottom edge 0
window h 896: pane 285x786, view 284x786, light pixels at right edge 2, at bottom edge 0
window h 895: pane 285x785, view 285x784, light pixels at right edge 0, at bottom edge 2
```

## After

```
window h 900: pane 285x790, view 285x790, light pixels at right edge 0, at bottom edge 0
window h 899: pane 285x789, view 285x789, light pixels at right edge 0, at bottom edge 0
window h 898: pane 285x788, view 285x788, light pixels at right edge 0, at bottom edge 0
window h 897: pane 285x787, view 285x787, light pixels at right edge 0, at bottom edge 0
window h 896: pane 285x786, view 285x786, light pixels at right edge 0, at bottom edge 0
window h 895: pane 285x785, view 285x785, light pixels at right edge 0, at bottom edge 0
```

Windows at fractional scale and a pane at other zooms were not run.
