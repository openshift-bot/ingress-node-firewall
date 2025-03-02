package nodefwloader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"unsafe"

	"github.com/cilium/ebpf/perf"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// ingressNodeFwEvents watch for eBPF events generated during XDP packet processing
func (infc *IngNodeFwController) ingressNodeFwEvents() {
	objs := infc.objs
	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, os.Interrupt, syscall.SIGTERM)

	// Open a perf event reader from userspace on the PERF_EVENT_ARRAY map
	// described in the eBPF C program.
	rd, err := perf.NewReader(objs.IngressNodeFirewallEventsMap, os.Getpagesize())
	if err != nil {
		log.Printf("Failed creating perf event reader: %q", err)
		return
	}
	logFile, ok := os.LookupEnv("EVENT_LOGGING_FILE")
	if !ok {
		log.Printf("EVENT_LOGGING_FILE env variable must be set")
		return
	}
	file, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("Cannot open events logging file at path %q: %v", logFile, err)
		return
	}
	eventsLogger := log.New(file, "", log.LstdFlags)

	go func() {
		// Wait for a signal and close the perf reader,
		// which will interrupt rd.Read() and make the program exit.
		<-stopper
		log.Println("Received signal, exiting program..")

		if err := rd.Close(); err != nil {
			log.Printf("Closing perf event reader: %q", err)
			return
		}
		if err := file.Close(); err != nil {
			log.Printf("Failed to close events log file")
			return
		}
	}()

	log.Printf("Listening for events..")

	// bpfEventHdrSt is generated by bpf2go.
	go func() {
		var eventHdr BpfEventHdrSt
		const eventHdrSize = unsafe.Sizeof(eventHdr)
		buf := make([]byte, eventHdrSize)

		for {
			record, err := rd.Read()
			if err != nil {
				if errors.Is(err, perf.ErrClosed) {
					return
				}
				log.Printf("Reading from perf event reader: %q", err)
				continue
			}

			if record.LostSamples != 0 {
				log.Printf("Perf event ring buffer full, dropped %d samples", record.LostSamples)
				continue
			}

			// read the event header
			if _, err := io.ReadFull(bytes.NewBuffer(record.RawSample), buf[:]); err != nil {
				log.Printf("Parsing perf event header err: %v", err)
				continue
			}
			// Note position of the bytes in the buf slice depends on the layout of bpfEventHdrSt struct
			eventHdr.IfId = binary.LittleEndian.Uint16(buf[0:2])
			eventHdr.RuleId = binary.LittleEndian.Uint16(buf[2:4])
			eventHdr.Action = buf[4]
			eventHdr.PktLength = binary.LittleEndian.Uint16(buf[6:8])
			packet := make([]byte, eventHdr.PktLength)
			// Parse the perf event entry into a bpfEvent structure.
			if err := binary.Read(bytes.NewBuffer(record.RawSample[eventHdrSize:]), binary.LittleEndian, &packet); err != nil {
				log.Printf("Parsing perf event packet header : %v", err)
				continue
			}
			// Look up the network interface by index.
			iface, err := net.InterfaceByIndex(int(eventHdr.IfId))
			if err != nil {
				log.Printf("lookup network iface %d: %s", eventHdr.IfId, err)
				continue
			}
			eventsLogger.Printf("\truleId %d action %s len %d if %s\n",
				eventHdr.RuleId, convertXdpActionToString(eventHdr.Action), eventHdr.PktLength, iface.Name)
			decodePacket := gopacket.NewPacket(packet, layers.LayerTypeEthernet, gopacket.Default)
			// check for IPv4
			if ip4Layer := decodePacket.Layer(layers.LayerTypeIPv4); ip4Layer != nil {
				ip, _ := ip4Layer.(*layers.IPv4)
				eventsLogger.Printf("\tipv4 src addr %s dst addr %s\n", ip.SrcIP.String(), ip.DstIP.String())
			}
			// check for IPv6
			if ip6Layer := decodePacket.Layer(layers.LayerTypeIPv6); ip6Layer != nil {
				ip, _ := ip6Layer.(*layers.IPv6)
				eventsLogger.Printf("\tipv6 src addr %s dst addr %s\n", ip.SrcIP.String(), ip.DstIP.String())
			}
			// check for TCP
			if tcpLayer := decodePacket.Layer(layers.LayerTypeTCP); tcpLayer != nil {
				tcp, _ := tcpLayer.(*layers.TCP)
				eventsLogger.Printf("\ttcp srcPort %d dstPort %d\n", tcp.SrcPort, tcp.DstPort)
			}
			// check for UDP
			if udpLayer := decodePacket.Layer(layers.LayerTypeUDP); udpLayer != nil {
				udp, _ := udpLayer.(*layers.UDP)
				eventsLogger.Printf("\tudp srcPort %d dstPort %d\n", udp.SrcPort, udp.DstPort)
			}
			// check fo SCTP
			if sctpLayer := decodePacket.Layer(layers.LayerTypeSCTP); sctpLayer != nil {
				sctp, _ := sctpLayer.(*layers.SCTP)
				eventsLogger.Printf("\tsctp srcPort %d dstPort %d\n", sctp.SrcPort, sctp.DstPort)
			}
			// check for ICMPv4
			if icmpv4Layer := decodePacket.Layer(layers.LayerTypeICMPv4); icmpv4Layer != nil {
				icmp, _ := icmpv4Layer.(*layers.ICMPv4)
				eventsLogger.Printf("\ticmpv4 type %d code %d\n", icmp.TypeCode.Type(), icmp.TypeCode.Code())
			}
			// check for ICMPV6
			if icmpv6Layer := decodePacket.Layer(layers.LayerTypeICMPv6); icmpv6Layer != nil {
				icmp, _ := icmpv6Layer.(*layers.ICMPv6)
				eventsLogger.Printf("\ticmpv6 type %d code %d\n", icmp.TypeCode.Type(), icmp.TypeCode.Code())
			}
		}
	}()
}

func convertXdpActionToString(action uint8) string {
	switch action {
	case xdpDeny:
		return "Drop"
	case xdpAllow:
		return "Allow"
	default:
		return fmt.Sprintf("Invalid action %d", action)
	}
}
