/*
 * tinytaptunnel v1.3 - Vanya A. Sergeev - vsergeev at gmail
 *
 * Point-to-Point Layer 2 tap interface tunnel over UDP/IP, with
 * MAC authentication. See README.md for more information.
 *
 */

package main

import (
    "fmt"
    "errors"
    "os"
    "net"
    "log"
    "time"
    "bytes"
    "hash"
    "crypto/sha256"
    "crypto/hmac"
    "crypto/rand"
    "encoding/binary"
    "encoding/base64"
    "encoding/hex"
    "syscall"
    "unsafe"
)

const (
    /* HMAC-SHA256 MAC Size */
    HMAC_SHA256_SIZE = sha256.Size
    /* Timestamp Size */
    TIMESTAMP_SIZE = 8

    /* Acceptable timestamp difference threshold in nS (3.0 seconds) */
    TIMESTAMP_DIFF_THRESHOLD int64 = 3.0*1e9

    /* UDP Payload MTU =
     *   Ethernet MTU (1500) - IPv4 Header (20) - UDP Header (8) = 1472 */
    UDP_MTU = 1472

    /* Tap MTU
     *   = UDP_MTU - Ethernet Header (14) - HMAC_SHA256_SIZE - TIMESTAMP_SIZE
     *   = 1418 */
    TAP_MTU = UDP_MTU - 14 - HMAC_SHA256_SIZE - TIMESTAMP_SIZE

    /* Debug level: 0 (off), 1 (report discarded frames), 2 (verbose) */
    DEBUG = 1
)

/**********************************************************************/
/*** Key file reading and generation ***/
/**********************************************************************/

/* The key file simply contains a base64 encoded random key.
 * The default random key size is HMAC_SHA256_SIZE. */

func keyfile_read(path string) (key []byte, e error) {
    var key_base64 []byte

    /* Attempt to open the key file for reading */
    keyfile, err := os.Open(path)
    if err != nil {
        return nil, err
    }
    defer keyfile.Close()

    /* Get the key file size */
    fi, err := keyfile.Stat()
    if err != nil {
        return nil, fmt.Errorf("Error getting key file size!: %s\n", err)
    }

    /* Read the base64 key */
    key_base64 = make([]byte, fi.Size())
    n, err := keyfile.Read(key_base64)
    if err != nil {
        return nil, fmt.Errorf("Error reading key file!: %s\n", err)
    }
    /* Trim whitespace */
    key_base64 = bytes.TrimSpace(key_base64)

    /* Decode the base64 key */
    key = make([]byte, base64.StdEncoding.DecodedLen(len(key_base64)))
    n, err = base64.StdEncoding.Decode(key, key_base64)
    if err != nil {
        return nil, fmt.Errorf("Error decoding base64 key file!: %s\n", err)
    }
    /* Truncate the key bytes to the right size */
    key = key[0:n]

    /* Check key size */
    if len(key) == 0 {
        return nil, fmt.Errorf("Error, invalid key in key file!")
    }

    return key, nil
}

func keyfile_generate(path string) (key []byte, e error) {
    /* Generate a random key */
    key = make([]byte, HMAC_SHA256_SIZE)
    n, err := rand.Read(key)
    if n != len(key) {
        return nil, fmt.Errorf("Error generating random key of size %d!\n", len(key))
    }

    /* Base64 encode the key */
    key_base64 := make([]byte, base64.StdEncoding.EncodedLen(len(key)))
    base64.StdEncoding.Encode(key_base64, key)

    /* Open the key file for writing */
    keyfile, err := os.Create(path)
    if err != nil {
        return nil, fmt.Errorf("Error opening key file for writing!: %s\n", err)
    }
    defer keyfile.Close()

    /* Write the base64 encoded key */
    _, err = keyfile.Write(key_base64)
    if err != nil {
        return nil, fmt.Errorf("Error writing base64 encoded key to keyfile!: %s\n", err)
    }

    return key, nil
}

/**********************************************************************/
/*** Frame Encapsulation ***/
/**********************************************************************/

/* Encapsulated Frame Format
 * | HMAC-SHA256 (32 bytes) | Nanosecond Timestamp (8 bytes) |
 * |             Plaintext Frame (1-1432 bytes)              |
 */

func encap_frame(frame []byte, hmac_h hash.Hash) (enc_frame []byte, invalid error) {
    /* Encode Big Endian representation of current nanosecond unix time */
    time_unixnano := time.Now().UnixNano()
    time_bytes := make([]byte, 8)
    binary.BigEndian.PutUint64(time_bytes, uint64(time_unixnano))

    /* Prepend the timestamp to the frame */
    timestamped_frame := append(time_bytes, frame...)

    /* Compute the HMAC-SHA256 of the timestamped frame */
    hmac_h.Reset()
    hmac_h.Write(timestamped_frame)

    /* Prepend the HMAC-SHA256 */
    enc_frame = append(hmac_h.Sum(nil), timestamped_frame...)

    return enc_frame, nil
}

func decap_frame(enc_frame []byte, hmac_h hash.Hash) (frame []byte, invalid error) {
    /* Check that the encapsulated frame size is valid */
    if len(enc_frame) < (TIMESTAMP_SIZE + HMAC_SHA256_SIZE + 1) {
        return nil, errors.New("Invalid encapsulated frame size!")
    }

    /* Verify the timestamp */
    time_bytes := enc_frame[HMAC_SHA256_SIZE:HMAC_SHA256_SIZE+TIMESTAMP_SIZE]
    time_unixnano := int64(binary.BigEndian.Uint64(time_bytes))
    curtime_unixnano := time.Now().UnixNano()
    if (curtime_unixnano - time_unixnano) > TIMESTAMP_DIFF_THRESHOLD {
        return nil, errors.New("Timestamp outside of acceptable range!")
    }

    /* Verify the HMAC-SHA256 */
    hmac_h.Reset()
    hmac_h.Write(enc_frame[HMAC_SHA256_SIZE:])
    if bytes.Compare(hmac_h.Sum(nil), enc_frame[0:HMAC_SHA256_SIZE]) != 0 {
        return nil, errors.New("Error verifying MAC!")
    }

    return enc_frame[HMAC_SHA256_SIZE+TIMESTAMP_SIZE:], nil
}

/**********************************************************************/
/*** Tap Device Open/Close/Read/Write ***/
/**********************************************************************/

type TapConn struct {
    fd     int
    ifname string
}

func (tap_conn *TapConn) Open(mtu uint) (err error) {
    /* Open the tap/tun device */
    tap_conn.fd, err = syscall.Open("/dev/net/tun", syscall.O_RDWR, syscall.S_IRUSR | syscall.S_IWUSR | syscall.S_IRGRP | syscall.S_IROTH)
    if err != nil {
        return fmt.Errorf("Error opening device /dev/net/tun!: %s", err)
    }

    /* Prepare a struct ifreq structure for TUNSETIFF with tap settings */
    /* IFF_TAP: tap device, IFF_NO_PI: no extra packet information */
    ifr_flags := uint16(syscall.IFF_TAP | syscall.IFF_NO_PI)
    /* FIXME: Assumes little endian */
    ifr_struct := make([]byte, 32)
    ifr_struct[16] = byte(ifr_flags)
    ifr_struct[17] = byte(ifr_flags >> 8)
    r0, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(tap_conn.fd), syscall.TUNSETIFF, uintptr(unsafe.Pointer(&ifr_struct[0])))
    if r0 != 0 {
        tap_conn.Close()
        return fmt.Errorf("Error setting tun/tap type!: %s", err)
    }

    /* Extract the assigned interface name into a string */
    tap_conn.ifname = string(ifr_struct[0:16])

    /* Create a raw socket for our tap interface, so we can set the MTU */
    tap_sockfd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, syscall.ETH_P_ALL)
    if err != nil {
        tap_conn.Close()
        return fmt.Errorf("Error creating packet socket!: %s", err)
    }
    /* We won't need the socket after we've set the MTU and brought the
     * interface up */
    defer syscall.Close(tap_sockfd)

    /* Bind the raw socket to our tap interface */
    err = syscall.BindToDevice(tap_sockfd, tap_conn.ifname)
    if err != nil {
        tap_conn.Close()
        return fmt.Errorf("Error binding packet socket to tap interface!: %s", err)
    }

    /* Prepare a ifreq structure for SIOCSIFMTU with MTU setting */
    ifr_mtu := mtu
    /* FIXME: Assumes little endian */
    ifr_struct[16] = byte(ifr_mtu)
    ifr_struct[17] = byte(ifr_mtu >> 8)
    ifr_struct[18] = byte(ifr_mtu >> 16)
    ifr_struct[19] = byte(ifr_mtu >> 24)
    r0, _, err = syscall.Syscall(syscall.SYS_IOCTL, uintptr(tap_sockfd), syscall.SIOCSIFMTU, uintptr(unsafe.Pointer(&ifr_struct[0])))
    if r0 != 0 {
        tap_conn.Close()
        return fmt.Errorf("Error setting MTU on tap interface!: %s", err)
    }

    /* Get the current interface flags in ifr_struct */
    r0, _, err = syscall.Syscall(syscall.SYS_IOCTL, uintptr(tap_sockfd), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifr_struct[0])))
    if r0 != 0 {
        tap_conn.Close()
        return fmt.Errorf("Error getting tap interface flags!: %s", err)
    }
    /* Update the interface flags to bring the interface up */
    /* FIXME: Assumes little endian */
    ifr_flags = uint16(ifr_struct[16]) | uint16(ifr_struct[17] << 8)
    ifr_flags |= syscall.IFF_UP | syscall.IFF_RUNNING
    ifr_struct[16] = byte(ifr_flags)
    ifr_struct[17] = byte(ifr_flags >> 8)
    r0, _, err = syscall.Syscall(syscall.SYS_IOCTL, uintptr(tap_sockfd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr_struct[0])))
    if r0 != 0 {
        tap_conn.Close()
        return fmt.Errorf("Error bringing up tap interface!: %s", err)
    }

    return nil
}

func (tap_conn *TapConn) Close() {
    syscall.Close(tap_conn.fd)
}

func (tap_conn *TapConn) Read(b []byte) (n int, err error) {
    return syscall.Read(tap_conn.fd, b)
}

func (tap_conn *TapConn) Write(b []byte) (n int, err error) {
    return syscall.Write(tap_conn.fd, b)
}

/**********************************************************************/
/** Phys to Tap Forwarding ***/
/**********************************************************************/

func forward_phys_to_tap(phys_conn *net.UDPConn, tap_conn *TapConn, peer_addr *net.UDPAddr, key []byte, chan_disc_peer chan net.UDPAddr) {
    /* Raw UDP packet received */
    packet := make([]byte, UDP_MTU)
    /* Decapsulated frame and error */
    var dec_frame []byte
    var invalid error = nil
    /* Peer address */
    var cur_peer_addr net.UDPAddr
    /* Peer discovery */
    var peer_discovery bool

    /* Initialize our HMAC-SHA256 hash context */
    hmac_h := hmac.New(sha256.New, key)

    /* If a peer was specified, fill in our peer information */
    if peer_addr != nil {
        cur_peer_addr.IP = peer_addr.IP
        cur_peer_addr.Port = peer_addr.Port
        log.Printf("Starting udp->tap forwarding with peer %s:%d...\n", cur_peer_addr.IP, cur_peer_addr.Port)
        peer_discovery = false
    } else {
        log.Printf("Starting udp->tap forwarding with peer discovery...\n")
        peer_discovery = true
    }

    for {
        /* Receive an encapsulated frame packet through UDP */
        n, raddr, err := phys_conn.ReadFromUDP(packet)
        if err != nil {
            log.Fatalf("Error reading from UDP socket!: %s\n", err)
        }

        /* If peer discovery is off, ensure the received packge is from our
         * specified peer */
        if !peer_discovery && (!raddr.IP.Equal(cur_peer_addr.IP) || raddr.Port != cur_peer_addr.Port) {
                continue
        }

        if DEBUG == 2 {
            log.Println("<- udp  | Encapsulated frame:")
            log.Printf( "        | from Peer %s:%d\n", raddr.IP, raddr.Port)
            log.Println("\n" + hex.Dump(packet[0:n]))
        }

        /* Decapsulate the frame, skip it if it's invalid */
        dec_frame, invalid = decap_frame(packet[0:n], hmac_h)
        if invalid != nil {
            if DEBUG >= 1 {
                log.Printf("<- udp  | Frame discarded! Size: %d, Reason: %s\n", n, invalid.Error())
                log.Printf("        | from Peer %s:%d\n", raddr.IP, raddr.Port)
                log.Println("\n" + hex.Dump(packet[0:n]))
            }
            continue
        }

        /* If peer discovery is on and the peer is new, save the discovered
         * peer */
        if peer_discovery && (!raddr.IP.Equal(cur_peer_addr.IP) || raddr.Port != cur_peer_addr.Port) {
            cur_peer_addr.IP = raddr.IP
            cur_peer_addr.Port = raddr.Port
            /* Send the new peer info to our forward_tap_to_phys() goroutine
             * via channel */
            chan_disc_peer <- cur_peer_addr

            if DEBUG >= 0 {
                log.Printf("Discovered peer %s:%d!\n", cur_peer_addr.IP, cur_peer_addr.Port)
            }
        }

        if DEBUG == 2 {
            log.Println("-> tap  | Decapsulated frame from peer:")
            log.Println("\n" + hex.Dump(dec_frame))
        }

        /* Forward the decapsulated frame to our tap interface */
        _, err = tap_conn.Write(dec_frame)
        if err != nil {
            log.Fatalf("Error writing to tap device!: %s\n", err)
        }
    }
}

/**********************************************************************/
/** Tap to Phys Forwarding ***/
/**********************************************************************/

func forward_tap_to_phys(phys_conn *net.UDPConn, tap_conn *TapConn, peer_addr *net.UDPAddr, key []byte, chan_disc_peer chan net.UDPAddr) {
    /* Raw tap frame received */
    //var frame []byte
    frame := make([]byte, TAP_MTU + 14)
    /* Encapsulated frame and error */
    var enc_frame []byte
    var invalid error = nil
    /* Peer address */
    var cur_peer_addr net.UDPAddr
    /* Peer discovery */
    var peer_discovery bool

    /* Initialize our HMAC-SHA256 hash context */
    hmac_h := hmac.New(sha256.New, key)

    /* If a peer was specified, fill in our peer information */
    if peer_addr != nil {
        cur_peer_addr.IP = peer_addr.IP
        cur_peer_addr.Port = peer_addr.Port
        peer_discovery = false
    } else {
        peer_discovery = true
        /* Otherwise, wait for the forward_phys_to_tap() goroutine to discover a peer */
        cur_peer_addr = <-chan_disc_peer
    }

    log.Printf("Starting tap->udp forwarding with peer %s:%d...\n", cur_peer_addr.IP, cur_peer_addr.Port)

    for {
        /* If peer discovery is on, check for any newly discovered peers */
        if peer_discovery {
            select {
                case cur_peer_addr = <-chan_disc_peer:
                    log.Printf("Starting tap->udp forwarding with peer %s:%d...\n", cur_peer_addr.IP, cur_peer_addr.Port)
                default:
            }
        }

        /* Read a raw frame from our tap device */
        n, err := tap_conn.Read(frame)
        if err != nil {
            log.Fatalf("Error reading from tap device!: %s\n", err)
        }

        if DEBUG == 2 {
            log.Println("<- tap  | Plaintext frame to peer:")
            log.Println("\n" + hex.Dump(frame[0:n]))
        }

        /* Encapsulate the frame, skip it if it's invalid */
        enc_frame, invalid = encap_frame(frame[0:n], hmac_h)
        if invalid != nil {
            if DEBUG >= 1 {
                log.Printf("-> udp  | Frame discarded! Size: %d, Reason: %s\n", n, invalid.Error())
                log.Println("\n" + hex.Dump(frame[0:n]))
            }
            continue
        }

        if DEBUG == 2 {
            log.Println("-> udp  | Encapsulated frame to peer:")
            log.Println("\n" + hex.Dump(enc_frame))
        }

        /* Send the encapsulated frame to our peer through UDP */
        _, err = phys_conn.WriteToUDP(enc_frame, &cur_peer_addr)
        if err != nil {
            log.Fatalf("Error writing to UDP socket!: %s\n", err)
        }
    }
}

/**********************************************************************/
/** Main ***/
/**********************************************************************/

func main() {
    if len(os.Args) != 3 && len(os.Args) != 4 {
        fmt.Println("tinytaptunnel v1.3\n")
        fmt.Printf("Usage: %s <key file> <local address> [peer address]\n\n", os.Args[0])
        fmt.Println("If no peer address is provided, tinytaptunnel will discover its peer by valid\nframes it authenticates and decodes.\n")
        fmt.Println("If the specified key file does not exist, it will be automatically generated\nwith secure random bytes.\n")
        os.Exit(1)
    }

    var key []byte

    /* Attempt to read the key file */
    key, err := keyfile_read(os.Args[1])
    /* If the error is file does not exist */
    if err != nil && os.IsNotExist(err) {
        /* Auto-generate the key file */
        key, err = keyfile_generate(os.Args[1])
        if err != nil {
            log.Fatalf("Error generating key file!: %s\n", err)
        }
    } else if err != nil {
        log.Fatalf("Error reading key file!: %s\n", err)
    }

    /* Parse & resolve local address */
    local_addr, err := net.ResolveUDPAddr("udp", os.Args[2])
    if err != nil {
        log.Fatalf("Error resolving local address!: %s\n", err)
    }

    /* Parse & resolve the peer address, if it was provided */
    var peer_addr *net.UDPAddr
    var chan_disc_peer chan net.UDPAddr

    if len(os.Args) == 4 {
        peer_addr, err = net.ResolveUDPAddr("udp", os.Args[3])
        if err != nil {
            log.Fatalf("Error resolving peer address!: %s\n", err)
        }
        chan_disc_peer = nil
    } else {
    /* Otherwise, prepare a channel that the forward_phys_to_tap() goroutine
     * will forward discovered peers through */
        peer_addr = nil
        chan_disc_peer = make(chan net.UDPAddr)
    }

    /* Create a UDP physical connection */
    phys_conn, err := net.ListenUDP("udp", local_addr)
    if err != nil {
        log.Fatalf("Error creating a UDP socket!: %s\n", err)
    }

    /* Create a tap interface */
    tap_conn := new(TapConn)
    err = tap_conn.Open(TAP_MTU)
    if err != nil {
        log.Fatalf("Error opening a tap device!: %s\n", err)
    }

    log.Printf("Created tunnel at interface %s with MTU %d\n\n", tap_conn.ifname, TAP_MTU)
    log.Println("Starting tinytaptunnel...")

    /* Run two goroutines for forwarding between interfaces */
    go forward_phys_to_tap(phys_conn, tap_conn, peer_addr, key, chan_disc_peer)
    forward_tap_to_phys(phys_conn, tap_conn, peer_addr, key, chan_disc_peer)
}

