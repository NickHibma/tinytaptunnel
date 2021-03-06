tinytaptunnel
=============

About
-----

tinytaptunnel creates a point-to-point layer 2 tunnel over UDP/IP, accessible via the generic tap interface at both peers. Any frames written at one peer's tap interface are tunneled to the other peer's tap interface, where they can be read, and vice-versa. In other words, tinytaptunnel creates a virtual ethernet between the tap interfaces of both peers.

tinytaptunnel authenticates all received frames by verifying the included HMAC-SHA256 MAC, computed with a preshared key. The MAC covers the encapsulated frame's contents, as well as an included 64-bit UNIX nanosecond timestamp.  tinytaptunnel will discard frames that contain a timestamp older than 5.0s of the current time to prevent spoofed replay attacks.

	tinytaptunnel Encapsulated Frame Format

       |                    IP Header (~20 bytes)                     |
       |                    UDP Header (8 bytes)                      |
	   | HMAC-SHA256 (32 bytes) | Nanosecond UNIX Timestamp (8 bytes) |
	   |               Plaintext Frame (1-1432 bytes)                 |
	

tinytaptunnel is lightweight, has easy command-line configuration, and is written in Go.

Since tinytaptunnel operates at layer 2, it can be used for layer 2 bridging with standard ethernet bridge tools like brctl, or layer 3 routing / NAT with standard routing tools like iptables. Convenient scripts to create these bridges or NAT are included in the scripts/ folder.

Other, more capable, tunneling software include [OpenSSH](http://openssh.com/), [OpenVPN](http://openvpn.net/), [n2n](http://www.ntop.org/products/n2n/), [tinc](http://www.tinc-vpn.org/), [stunnel](https://www.stunnel.org/), [socat](http://www.dest-unreach.org/socat/) and so on.

Building
--------

	$ go build

will build the tinytaptunnel binary with the gc Go compiler.

Running
-------

Program Usage:

    tinytaptunnel v1.3
    
    Usage: ./tinytaptunnel <key file> <local address> [peer address]
    
    If no peer address is provided, tinytaptunnel will discover its peer by valid
    frames it authenticates and decodes.
    
    If the specified key file does not exist, it will be automatically generated
    with secure random bytes.
    

The key file contains a preshared base64 encoded key. If the specified key file does not exist, tinytaptunnel will generate it with a 32 byte securely randomly generated key.

##### Example with Explicit Addressing

Peer 1

	$ sudo ./tinytaptunnel preshared.key :9123 peer2_address:9123

Peer 2

	$ sudo ./tinytaptunnel preshared.key :9123 peer1_address:9123

##### Example with Peer Discovery

Peer 1

	$ sudo ./tinytaptunnel preshared.key :9123

Peer 2

	$ sudo ./tinytaptunnel preshared.key :9123 peer1_address:9123

Peer 1 will discover peer 2's IP address and port upon authenticating and decoding a valid frame from peer 2. Until discovery, peer 1 cannot tunnel frames to peer 2.

Using tinytaptunnel for a Point-to-Point Static IP Connection
-------------------------------------------------------------

Configuring the provided tap interface at each peer with static IP addresses on the same private subnet will allow for a point-to-point tunneled static IP connection.

##### Example Static IP Configuration

Peer 1

	$ sudo ./tinytaptunnel preshared.key :9123 peer2_address:9123
	$ sudo ifconfig tap0 10.1.2.3

Peer 2

	$ sudo ./tinytaptunnel preshared.key :9123 peer1_address:9123
	$ sudo ifconfig tap0 10.1.2.4

Peer 1 should now be able to ping peer 2 at 10.1.2.4 through the tunnel, and peer 2 should be able to ping peer 1 at 10.1.2.3.

Using tinytaptunnel for a Layer 2 Bridge
----------------------------------------

Bridging the provided tap interface with a physical interface at one peer will give the other peer layer 2 access to the bridging peer's local network.  The other peer can essentially act as another host on the bridging peer's local layer 2 network.

##### Example Configuration for Peer 1 Sharing their Local Network (Layer 2)

Peer 1

	$ sudo ./tinytaptunnel preshared.key :9123
	$ sudo brctl addbr vpnbridge
	$ sudo brctl addif vpnbridge tap0
	$ sudo brctl addif vpnbridge eth0
	$ sudo ifconfig vpnbridge up
	  (these bridge commands are also contained in scripts/l2_bridge_up.sh
	   for convenience)

Peer 2

	$ sudo ./tinytaptunnel preshared.key :9123 peer1_address:9123

Peer 2 should now be able to ARP other hosts on peer 1's local network; request a local address from peer 1's router with DHCP; or in general, participate in peer 1's local IP network after appropriate address configuration.

Using tinytaptunnel for a Layer 3 NAT
--------------------------------------

Configuring one peer to act as a NAT router for the point-to-point static IP connection with the other peer enables the other to make IP connections with hosts in the routing peer's local private network.

##### Example Configuration for Peer 1 Sharing their Local Network (Layer 3)

In this example, peer 1 is on the 192.168.1.0/24 subnet, and both peers have a point-to-point connection via tinytaptunnel with static IP addresses on the 10.0.0.0/8 subnet.

Peer 1

	$ sudo ./tinytaptunnel preshared.key :9123
	$ sudo ifconfig tap0 10.1.2.3
	$ sudo sysctl -w net.ipv4.ip_forward=1
	$ sudo iptables -t nat -A POSTROUTING -j MASQUERADE -o eth0
	$ sudo iptables -A FORWARD -i tap0 -o eth0 -j ACCEPT
	  (these iptables commands are also contained in scripts/l3_nat_up.sh
	   for convenience)

Peer 2

	$ sudo ./tinytaptunnel preshared.key :9123 peer1_address:9123
	$ sudo ifconfig tap0 10.1.2.4
	$ sudo ip route add 192.168.1.0/24 via 10.1.2.3 dev tap0

Now peer 2 should be able to communicate with hosts in peer 1's 192.168.1.0/24 subnet via the route through peer 1 at 10.1.2.3. Peer 1 will translate / masquerade the source address to make the connections on behalf of peer 2.

Limitations of tinytaptunnel
----------------------------

Due to the MAC and timestamp overhead in encapsulated frames, tinytaptunnel presents a tap interface with an MTU of 1418. This limited MTU may lead to some performance loss due to packet fragmentation.

Closing Remarks
---------------

tinytaptunnel is MIT licensed. See the provided LICENSE file.

Feel free to report any issues, bug reports, or suggestions at [github](https://github.com/vsergeev/tinytaptunnel/issues) or by email at vsergeev at gmail.

