Output setup.mp4
Source common.tape
Env TOUR_DELAY "800ms"
Hide
Type "clear && bin/substrate-aks -test.run TestTour 2>/dev/null"
Enter
Wait+Screen@20s /Quick start/
Show
Sleep 3s
Enter
Wait+Screen@10s /Valid cluster name/
Sleep 1.2s
Enter
Sleep 1.2s
Enter
Sleep 1s
Enter
Wait+Screen@20s /billable installation/
Sleep 1.2s
Down 6
Sleep 1.5s
Type "y"
Wait+Screen@20s /2 to create/
Sleep 2s
Type "y"
Wait+Screen@40s /11 to create/
Sleep 3s
Type "y"
Wait+Screen@40s /Import and select this kube-context/
Sleep 1.5s
Type "y"
Sleep 2s
Sleep 6s
