// Copyright (c) 2019 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nodestatus

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"reflect"

	"github.com/olekukonko/tablewriter"
	gobgp "github.com/osrg/gobgp/client"
	"github.com/osrg/gobgp/packet/bgp"
	"github.com/shirou/gopsutil/process"
	log "github.com/sirupsen/logrus"
)

func statusHandler(w http.ResponseWriter, r *http.Request) {
	Status(w)
}

func Run() {
	http.HandleFunc("/status/", statusHandler)
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// Status prints status of the node and returns error (if any)
func Status(w http.ResponseWriter) {
	// Must run this command as root to be able to connect to BIRD sockets
	err := enforceRoot()
	if err != nil {
		fmt.Fprintln(w, err)
		return
	}

	// Go through running processes and check if `calico-felix` processes is not running
	processes, err := process.Processes()
	if err != nil {
		fmt.Fprintln(w, err)
	}

	// For older versions of calico/node, the process was called `calico-felix`. Newer ones use `calico-node -felix`.
	if !psContains([]string{"calico-felix"}, processes) && !psContains([]string{"calico-node", "-felix"}, processes) {
		// Return and print message if calico-node is not running
		fmt.Fprintf(w, "Calico process is not running.\n")
		return
	}

	fmt.Fprintf(w, "Calico process is running.\n")

	if psContains([]string{"bird"}, processes) || psContains([]string{"bird6"}, processes) {
		// Check if birdv4 process is running, print the BGP peer table if it is, else print a warning
		if psContains([]string{"bird"}, processes) {
			printBIRDPeers(w, "4")
		} else {
			fmt.Fprintf(w, "\nINFO: BIRDv4 process: 'bird' is not running.\n")
		}
		// Check if birdv6 process is running, print the BGP peer table if it is, else print a warning
		if psContains([]string{"bird6"}, processes) {
			printBIRDPeers(w, "6")
		} else {
			fmt.Fprintf(w, "\nINFO: BIRDv6 process: 'bird6' is not running.\n")
		}
	} else if psContains([]string{"calico-bgp-daemon"}, processes) {
		printGoBGPPeers(w, "4")
		printGoBGPPeers(w, "6")
	} else {
		fmt.Fprintf(w, "\nNone of the BGP backend processes (BIRD or GoBGP) are running.\n")
	}

	// Have to manually enter an empty line because the table print
	// library prints the last line, so can't insert a '\n' there
	fmt.Fprintln(w)
}

func psContains(proc []string, procList []*process.Process) bool {
	for _, p := range procList {
		cmds, err := p.CmdlineSlice()
		if err != nil {
			// Failed to get CLI arguments for this process.
			// Maybe it doesn't exist any more - move on to the next one.
			log.WithError(err).Debug("Error getting CLI arguments")
			continue
		}
		var match bool
		for i, p := range proc {
			if i >= len(cmds) {
				break
			} else if cmds[i] == p {
				match = true
			}
		}

		// If we got a match, return true. Otherwise, try the next
		// process in the list.
		if match {
			return true
		}
	}
	return false
}

// Check for Word_<IP> where every octate is seperated by "_", regardless of IP protocols
// Example match: "Mesh_192_168_56_101" or "Mesh_fd80_24e2_f998_72d7__2"
var bgpPeerRegex = regexp.MustCompile(`^(Global|Node|Mesh)_(.+)$`)

// Mapping the BIRD/GoBGP type extracted from the peer name to the display type.
var bgpTypeMap = map[string]string{
	"Global": "global",
	"Mesh":   "node-to-node mesh",
	"Node":   "node specific",
}

// Timeout for querying BIRD
var birdTimeOut = 2 * time.Second

// Expected BIRD protocol table columns
var birdExpectedHeadings = []string{"name", "proto", "table", "state", "since", "info"}

// bgpPeer is a structure containing details about a BGP peer.
type bgpPeer struct {
	PeerIP   string
	PeerType string
	State    string
	Since    string
	BGPState string
	Info     string
}

// Unmarshal a peer from a line in the BIRD protocol output.  Returns true if
// successful, false otherwise.
func (b *bgpPeer) unmarshalBIRD(line, ipSep string) bool {
	// Split into fields.  We expect at least 6 columns:
	// 	name, proto, table, state, since and info.
	// The info column contains the BGP state plus possibly some additional
	// info (which will be columns > 6).
	//
	// Peer names will be of the format described by bgpPeerRegex.
	log.Debugf("Parsing line: %s", line)
	columns := strings.Fields(line)
	if len(columns) < 6 {
		log.Debugf("Not a valid line: fewer than 6 columns")
		return false
	}
	if columns[1] != "BGP" {
		log.Debugf("Not a valid line: protocol is not BGP")
		return false
	}

	// Check the name of the peer is of the correct format.  This regex
	// returns two components:
	// -  A type (Global|Node|Mesh) which we can map to a display type
	// -  An IP address (with _ separating the octets)
	sm := bgpPeerRegex.FindStringSubmatch(columns[0])
	if len(sm) != 3 {
		log.Debugf("Not a valid line: peer name '%s' is not correct format", columns[0])
		return false
	}
	var ok bool
	b.PeerIP = strings.Replace(sm[2], "_", ipSep, -1)
	if b.PeerType, ok = bgpTypeMap[sm[1]]; !ok {
		log.Debugf("Not a valid line: peer type '%s' is not recognized", sm[1])
		return false
	}

	// Store remaining columns (piecing back together the info string)
	b.State = columns[3]
	b.Since = columns[4]
	b.BGPState = columns[5]
	if len(columns) > 6 {
		b.Info = strings.Join(columns[6:], " ")
	}

	return true
}

// printBIRDPeers queries BIRD and displays the local peers in table format.
func printBIRDPeers(w http.ResponseWriter, ipv string) {
	log.Debugf("Print BIRD peers for IPv%s", ipv)
	birdSuffix := ""
	if ipv == "6" {
		birdSuffix = "6"
	}

	fmt.Fprintf(w, "\nIPv%s BGP status\n", ipv)

	// Try connecting to the bird socket in `/var/run/calico/` first to get the data
	c, err := net.Dial("unix", fmt.Sprintf("/var/run/calico/bird%s.ctl", birdSuffix))
	if err != nil {
		// If that fails, try connecting to bird socket in `/var/run/bird` (which is the
		// default socket location for bird install) for non-containerized installs
		log.Debugln("Failed to connect to BIRD socket in /var/run/calic, trying /var/run/bird")
		c, err = net.Dial("unix", fmt.Sprintf("/var/run/bird/bird%s.ctl", birdSuffix))
		if err != nil {
			fmt.Fprintf(w, "Error querying BIRD: unable to connect to BIRDv%s socket: %v", ipv, err)
			return
		}
	}
	defer c.Close()

	// To query the current state of the BGP peers, we connect to the BIRD
	// socket and send a "show protocols" message.  BIRD responds with
	// peer data in a table format.
	//
	// Send the request.
	_, err = c.Write([]byte("show protocols\n"))
	if err != nil {
		fmt.Fprintf(w, "Error executing command: unable to write to BIRD socket: %s\n", err)
		return
	}

	// Scan the output and collect parsed BGP peers
	log.Debugln("Reading output from BIRD")
	peers, err := scanBIRDPeers(ipv, c)
	if err != nil {
		fmt.Fprintf(w, "Error executing command: %v", err)
		return
	}

	// If no peers were returned then just print a message.
	if len(peers) == 0 {
		fmt.Fprintf(w, "No IPv%s peers found.\n", ipv)
		return
	}

	// Finally, print the peers.
	printPeers(peers)
}

// scanBIRDPeers scans through BIRD output to return a slice of bgpPeer
// structs.
//
// We split this out from the main printBIRDPeers() function to allow us to
// test this processing in isolation.
func scanBIRDPeers(ipv string, conn net.Conn) ([]bgpPeer, error) {
	// Determine the separator to use for an IP address, based on the
	// IP version.
	ipSep := "."
	if ipv == "6" {
		ipSep = ":"
	}

	// The following is sample output from BIRD
	//
	// 	0001 BIRD 1.5.0 ready.
	// 	2002-name     proto    table    state  since       info
	// 	1002-kernel1  Kernel   master   up     2016-11-21
	//  	 device1  Device   master   up     2016-11-21
	//  	 direct1  Direct   master   up     2016-11-21
	//  	 Mesh_172_17_8_102 BGP      master   up     2016-11-21  Established
	// 	0000
	scanner := bufio.NewScanner(conn)
	peers := []bgpPeer{}

	// Set a time-out for reading from the socket connection.
	conn.SetReadDeadline(time.Now().Add(birdTimeOut))

	for scanner.Scan() {
		// Process the next line that has been read by the scanner.
		str := scanner.Text()
		log.Debugf("Read: %s\n", str)

		if strings.HasPrefix(str, "0000") {
			// "0000" means end of data
			break
		} else if strings.HasPrefix(str, "0001") {
			// "0001" code means BIRD is ready.
		} else if strings.HasPrefix(str, "2002") {
			// "2002" code means start of headings
			f := strings.Fields(str[5:])
			if !reflect.DeepEqual(f, birdExpectedHeadings) {
				return nil, errors.New("unknown BIRD table output format")
			}
		} else if strings.HasPrefix(str, "1002") {
			// "1002" code means first row of data.
			peer := bgpPeer{}
			if peer.unmarshalBIRD(str[5:], ipSep) {
				peers = append(peers, peer)
			}
		} else if strings.HasPrefix(str, " ") {
			// Row starting with a " " is another row of data.
			peer := bgpPeer{}
			if peer.unmarshalBIRD(str[1:], ipSep) {
				peers = append(peers, peer)
			}
		} else {
			// Format of row is unexpected.
			return nil, errors.New("unexpected output line from BIRD")
		}

		// Before reading the next line, adjust the time-out for
		// reading from the socket connection.
		conn.SetReadDeadline(time.Now().Add(birdTimeOut))
	}

	return peers, scanner.Err()
}

// printGoBGPPeers queries GoBGP and displays the local peers in table format.
func printGoBGPPeers(w http.ResponseWriter, ipv string) {
	client, err := gobgp.New("")
	if err != nil {
		fmt.Fprintf(w, "Error creating gobgp client: %s\n", err)
		return
	}
	defer client.Close()

	afi := bgp.AFI_IP
	if ipv == "6" {
		afi = bgp.AFI_IP6
	}

	fmt.Fprintf(w, "\nIPv%s BGP status\n", ipv)

	neighbors, err := client.ListNeighborByTransport(afi)
	if err != nil {
		fmt.Fprintf(w, "Error retrieving neighbor info: %s\n", err)
		return
	}

	formatTimedelta := func(d int64) string {
		u := uint64(d)
		neg := d < 0
		if neg {
			u = -u
		}
		secs := u % 60
		u /= 60
		mins := u % 60
		u /= 60
		hours := u % 24
		days := u / 24

		if days == 0 {
			return fmt.Sprintf("%02d:%02d:%02d", hours, mins, secs)
		} else {
			return fmt.Sprintf("%dd ", days) + fmt.Sprintf("%02d:%02d:%02d", hours, mins, secs)
		}
	}

	now := time.Now()
	peers := make([]bgpPeer, 0, len(neighbors))

	for _, n := range neighbors {
		ipString := n.Config.NeighborAddress
		description := n.Config.Description
		adminState := string(n.State.AdminState)
		sessionState := strings.Title(string(n.State.SessionState))

		timeStr := "never"
		if n.Timers.State.Uptime != 0 {
			t := int(n.Timers.State.Downtime)
			if sessionState == "Established" {
				t = int(n.Timers.State.Uptime)
			}
			timeStr = formatTimedelta(int64(now.Sub(time.Unix(int64(t), 0)).Seconds()))
		}

		sm := bgpPeerRegex.FindStringSubmatch(description)
		if len(sm) != 3 {
			log.Debugf("Not a valid line: peer name '%s' is not recognized", description)
			continue
		}
		var ok bool
		var typ string
		if typ, ok = bgpTypeMap[sm[1]]; !ok {
			log.Debugf("Not a valid line: peer type '%s' is not recognized", sm[1])
			continue
		}

		peers = append(peers, bgpPeer{
			PeerIP:   ipString,
			PeerType: typ,
			State:    adminState,
			Since:    timeStr,
			BGPState: sessionState,
		})
	}

	// If no peers were returned then just print a message.
	if len(peers) == 0 {
		fmt.Fprintf(w, "No IPv%s peers found.\n", ipv)
		return
	}

	// Finally, print the peers.
	printPeers(peers)
}

// TODO: Need to figure out how to get tablewriter to write to the ResponseWriter
// printPeers prints out the slice of peers in table format.
func printPeers(peers []bgpPeer) {
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Peer address", "Peer type", "State", "Since", "Info"})

	for _, peer := range peers {
		info := peer.BGPState
		if peer.Info != "" {
			info += " " + peer.Info
		}
		row := []string{
			peer.PeerIP,
			peer.PeerType,
			peer.State,
			peer.Since,
			info,
		}
		table.Append(row)
	}

	table.Render()
}

func enforceRoot() error {
	// Make sure the command is run with super user priviladges
	if os.Getuid() != 0 {
		return errors.New("Need super user privileges: Operation not permitted")
	}
	return nil
}
