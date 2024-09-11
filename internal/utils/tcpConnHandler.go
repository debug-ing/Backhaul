package utils

import (
	"errors"
	"io"
	"net"

	"github.com/sirupsen/logrus"
)

func ConnectionHandler(from net.Conn, to net.Conn, logger *logrus.Logger, usage *Usage, remotePort int, sniffing bool) {
	done := make(chan struct{})

	go func() {
		defer close(done)
		transferData(from, to, logger, usage, remotePort, sniffing)
	}()

	transferData(to, from, logger, usage, remotePort, sniffing)

	<-done

	from.Close()
	to.Close()
}

// Using direct Read and Write for transferring data
func transferData(from net.Conn, to net.Conn, logger *logrus.Logger, usage *Usage, remotePort int, sniffing bool) {
	buf := make([]byte, 16*1024) // 16K
	for {
		// Read data from the source connection
		r, err := from.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				logger.Trace("reader stream closed or EOF received")
			} else {
				logger.Trace("unable to read from the connection: ", err)
			}

			from.Close()
			to.Close()
			return
		}

		totalWritten := 0
		for totalWritten < r {
			// Write data to the destination connection
			w, err := to.Write(buf[totalWritten:r])
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					logger.Trace("writer stream closed or EOF received")
				} else {
					logger.Trace("unable to write to the connection: ", err)
				}
				from.Close()
				to.Close()
				break
			}
			totalWritten += w
		}

		logger.Tracef("read data: %d bytes, written data: %d bytes", r, totalWritten)
		if sniffing {
			go usage.AddOrUpdatePort(remotePort, uint64(totalWritten))
		}
	}

}
